/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package secretmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/googleapis/gax-go/v2"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
	"google.golang.org/grpc/codes"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	"github.com/external-secrets/external-secrets/pkg/find"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

const (
	CloudPlatformRole                         = "https://www.googleapis.com/auth/cloud-platform"
	defaultVersion                            = "latest"
	errGCPSMStore                             = "received invalid GCPSM SecretStore resource"
	errUnableGetCredentials                   = "unable to get credentials: %w"
	errClientClose                            = "unable to close SecretManager client: %w"
	errMissingStoreSpec                       = "invalid: missing store spec"
	errInvalidClusterStoreMissingSAKNamespace = "invalid ClusterSecretStore: missing GCP SecretAccessKey Namespace"
	errInvalidClusterStoreMissingSANamespace  = "invalid ClusterSecretStore: missing GCP Service Account Namespace"
	errFetchSAKSecret                         = "could not fetch SecretAccessKey secret: %w"
	errMissingSAK                             = "missing SecretAccessKey"
	errUnableProcessJSONCredentials           = "failed to process the provided JSON credentials: %w"
	errUnableCreateGCPSMClient                = "failed to create GCP secretmanager client: %w"
	errUninitalizedGCPProvider                = "provider GCP is not initialized"
	errClientGetSecretAccess                  = "unable to access Secret from SecretManager Client: %w"
	errJSONSecretUnmarshal                    = "unable to unmarshal secret: %w"

	errInvalidStore           = "invalid store"
	errInvalidStoreSpec       = "invalid store spec"
	errInvalidStoreProv       = "invalid store provider"
	errInvalidGCPProv         = "invalid gcp secrets manager provider"
	errInvalidAuthSecretRef   = "invalid auth secret ref: %w"
	errInvalidWISARef         = "invalid workload identity service account reference: %w"
	errUnexpectedFindOperator = "unexpected find operator"
)

var log = ctrl.Log.WithName("provider").WithName("gcp").WithName("secretsmanager")

type GoogleSecretManagerClient interface {
	AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
	ListSecrets(ctx context.Context, req *secretmanagerpb.ListSecretsRequest, opts ...gax.CallOption) *secretmanager.SecretIterator
	AddSecretVersion(ctx context.Context, req *secretmanagerpb.AddSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.SecretVersion, error)
	CreateSecret(ctx context.Context, req *secretmanagerpb.CreateSecretRequest, opts ...gax.CallOption) (*secretmanagerpb.Secret, error)
	Close() error
	GetSecret(ctx context.Context, req *secretmanagerpb.GetSecretRequest, opts ...gax.CallOption) (*secretmanagerpb.Secret, error)
}

/*
 Currently, GCPSM client has a limitation around how concurrent connections work
 This limitation causes memory leaks due to random disconnects from living clients
 and also payload switches when sending a call (such as using a credential from one
 thread to ask secrets from another thread).
 A Mutex was implemented to make sure only one connection can be in place at a time.
*/
var useMu = sync.Mutex{}

// https://github.com/external-secrets/external-secrets/issues/644
var _ esv1beta1.SecretsClient = &ProviderGCP{}
var _ esv1beta1.Provider = &ProviderGCP{}

// ProviderGCP is a provider for GCP Secret Manager.
type ProviderGCP struct {
	ProjectID           string
	SecretManagerClient GoogleSecretManagerClient
	gClient             *gClient
}

type gClient struct {
	kube      kclient.Client
	store     *esv1beta1.GCPSMProvider
	namespace string
	storeKind string

	workloadIdentity *workloadIdentity
}

func (c *gClient) getTokenSource(ctx context.Context, store esv1beta1.GenericStore, kube kclient.Client, namespace string) (oauth2.TokenSource, error) {
	ts, err := serviceAccountTokenSource(ctx, store, kube, namespace)
	if ts != nil || err != nil {
		return ts, err
	}
	ts, err = c.workloadIdentity.TokenSource(ctx, store, kube, namespace)
	if ts != nil || err != nil {
		return ts, err
	}

	return google.DefaultTokenSource(ctx, CloudPlatformRole)
}

func (c *gClient) Close() error {
	return c.workloadIdentity.Close()
}

func serviceAccountTokenSource(ctx context.Context, store esv1beta1.GenericStore, kube kclient.Client, namespace string) (oauth2.TokenSource, error) {
	spec := store.GetSpec()
	if spec == nil || spec.Provider.GCPSM == nil {
		return nil, fmt.Errorf(errMissingStoreSpec)
	}
	sr := spec.Provider.GCPSM.Auth.SecretRef
	if sr == nil {
		return nil, nil
	}
	storeKind := store.GetObjectKind().GroupVersionKind().Kind
	credentialsSecret := &v1.Secret{}
	credentialsSecretName := sr.SecretAccessKey.Name
	objectKey := types.NamespacedName{
		Name:      credentialsSecretName,
		Namespace: namespace,
	}

	// only ClusterStore is allowed to set namespace (and then it's required)
	if storeKind == esv1beta1.ClusterSecretStoreKind {
		if credentialsSecretName != "" && sr.SecretAccessKey.Namespace == nil {
			return nil, fmt.Errorf(errInvalidClusterStoreMissingSAKNamespace)
		} else if credentialsSecretName != "" {
			objectKey.Namespace = *sr.SecretAccessKey.Namespace
		}
	}
	err := kube.Get(ctx, objectKey, credentialsSecret)
	if err != nil {
		return nil, fmt.Errorf(errFetchSAKSecret, err)
	}
	credentials := credentialsSecret.Data[sr.SecretAccessKey.Key]
	if (credentials == nil) || (len(credentials) == 0) {
		return nil, fmt.Errorf(errMissingSAK)
	}
	config, err := google.JWTConfigFromJSON(credentials, CloudPlatformRole)
	if err != nil {
		return nil, fmt.Errorf(errUnableProcessJSONCredentials, err)
	}
	return config.TokenSource(ctx), nil
}

// Capabilities return the provider supported capabilities (ReadOnly, WriteOnly, ReadWrite).
func (sm *ProviderGCP) Capabilities() esv1beta1.SecretStoreCapabilities {
	return esv1beta1.SecretStoreReadWrite
}

// NewClient constructs a GCP Provider.
func (sm *ProviderGCP) NewClient(ctx context.Context, store esv1beta1.GenericStore, kube kclient.Client, namespace string) (esv1beta1.SecretsClient, error) {
	storeSpec := store.GetSpec()
	if storeSpec == nil || storeSpec.Provider == nil || storeSpec.Provider.GCPSM == nil {
		return nil, fmt.Errorf(errGCPSMStore)
	}
	storeSpecGCPSM := storeSpec.Provider.GCPSM

	useMu.Lock()
	wi, err := newWorkloadIdentity(ctx)
	if err != nil {
		useMu.Unlock()
		return nil, fmt.Errorf("unable to initialize workload identity")
	}

	cliStore := gClient{
		kube:             kube,
		store:            storeSpecGCPSM,
		namespace:        namespace,
		storeKind:        store.GetObjectKind().GroupVersionKind().Kind,
		workloadIdentity: wi,
	}
	sm.gClient = &cliStore
	defer func() {
		// closes IAMClient to prevent gRPC connection leak in case of an error.
		if sm.SecretManagerClient == nil {
			_ = sm.gClient.Close()
		}
	}()

	sm.ProjectID = cliStore.store.ProjectID

	ts, err := cliStore.getTokenSource(ctx, store, kube, namespace)
	if err != nil {
		useMu.Unlock()
		return nil, fmt.Errorf(errUnableCreateGCPSMClient, err)
	}

	// check if we can get credentials
	_, err = ts.Token()
	if err != nil {
		useMu.Unlock()
		return nil, fmt.Errorf(errUnableGetCredentials, err)
	}

	clientGCPSM, err := secretmanager.NewClient(ctx, option.WithTokenSource(ts))
	if err != nil {
		useMu.Unlock()
		return nil, fmt.Errorf(errUnableCreateGCPSMClient, err)
	}
	sm.SecretManagerClient = clientGCPSM
	return sm, nil
}

// SetSecret pushes a kubernetes secret key into gcp provider Secret.
// funcName(variable type_of_variable, ...)
func (sm *ProviderGCP) SetSecret(ctx context.Context, payload []byte, remoteRef esv1beta1.PushRemoteRef) error {
	createSecretReq := &secretmanagerpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", sm.ProjectID),
		SecretId: remoteRef.GetRemoteKey(),
		Secret: &secretmanagerpb.Secret{
			Labels: map[string]string{
				"managed-by": "external-secrets",
			},
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
		},
	}

	var gcpSecret *secretmanagerpb.Secret
	var err error

	gcpSecret, err = sm.SecretManagerClient.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s", sm.ProjectID, remoteRef.GetRemoteKey()),
	})

	var gErr *apierror.APIError

	if err != nil && errors.As(err, &gErr) {
		if gErr.GRPCStatus().Code() == codes.NotFound {
			gcpSecret, err = sm.SecretManagerClient.CreateSecret(ctx, createSecretReq)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	manager, ok := gcpSecret.Labels["managed-by"]

	if !ok || manager != "external-secrets" {
		return fmt.Errorf("secret %v is not managed by external secrets", remoteRef.GetRemoteKey())
	}

	gcpVersion, err := sm.SecretManagerClient.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/latest", sm.ProjectID, remoteRef.GetRemoteKey()),
	})

	if errors.As(err, &gErr) {
		if err != nil && gErr.GRPCStatus().Code() != codes.NotFound {
			return err
		}
	} else if err != nil {
		return err
	}

	if gcpVersion != nil && gcpVersion.Payload != nil && string(payload) == string(gcpVersion.Payload.Data) {
		return nil
	}

	addSecretVersionReq := &secretmanagerpb.AddSecretVersionRequest{
		Parent: fmt.Sprintf("projects/%s/secrets/%s", sm.ProjectID, remoteRef.GetRemoteKey()),
		Payload: &secretmanagerpb.SecretPayload{
			Data: payload,
		},
	}

	_, err = sm.SecretManagerClient.AddSecretVersion(ctx, addSecretVersionReq)

	if err != nil {
		return err
	}

	return nil
}

// GetAllSecrets syncs multiple secrets from gcp provider into a single Kubernetes Secret.
func (sm *ProviderGCP) GetAllSecrets(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	if ref.Name != nil {
		return sm.findByName(ctx, ref)
	}
	if len(ref.Tags) > 0 {
		return sm.findByTags(ctx, ref)
	}

	return nil, errors.New(errUnexpectedFindOperator)
}

func (sm *ProviderGCP) findByName(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	// regex matcher
	matcher, err := find.New(*ref.Name)
	if err != nil {
		return nil, err
	}
	req := &secretmanagerpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", sm.ProjectID),
	}
	if ref.Path != nil {
		req.Filter = fmt.Sprintf("name:%s", *ref.Path)
	}
	// Call the API.
	it := sm.SecretManagerClient.ListSecrets(ctx, req)
	secretMap := make(map[string][]byte)
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list secrets: %w", err)
		}
		log.V(1).Info("gcp sm findByName found", "secrets", strconv.Itoa(it.PageInfo().Remaining()))
		key := sm.trimName(resp.Name)
		// If we don't match we skip.
		// Also, if we have path, and it is not at the beguining we skip.
		// We have to check if path is at the beguining of the key because
		// there is no way to create a `name:%s*` (starts with) filter
		// At https://cloud.google.com/secret-manager/docs/filtering you can use `*`
		// but not like that it seems.
		if !matcher.MatchName(key) || (ref.Path != nil && !strings.HasPrefix(key, *ref.Path)) {
			continue
		}
		log.V(1).Info("gcp sm findByName matches", "name", resp.Name)
		secretMap[key], err = sm.getData(ctx, key)
		if err != nil {
			return nil, err
		}
	}

	return utils.ConvertKeys(ref.ConversionStrategy, secretMap)
}

func (sm *ProviderGCP) getData(ctx context.Context, key string) ([]byte, error) {
	dataRef := esv1beta1.ExternalSecretDataRemoteRef{
		Key: key,
	}
	data, err := sm.GetSecret(ctx, dataRef)
	if err != nil {
		return []byte(""), err
	}
	return data, nil
}

func (sm *ProviderGCP) findByTags(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	var tagFilter string
	for k, v := range ref.Tags {
		tagFilter = fmt.Sprintf("%slabels.%s=%s ", tagFilter, k, v)
	}
	tagFilter = strings.TrimSuffix(tagFilter, " ")
	if ref.Path != nil {
		tagFilter = fmt.Sprintf("%s name:%s", tagFilter, *ref.Path)
	}
	req := &secretmanagerpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", sm.ProjectID),
	}
	log.V(1).Info("gcp sm findByTags", "tagFilter", tagFilter)
	req.Filter = tagFilter
	// Call the API.
	it := sm.SecretManagerClient.ListSecrets(ctx, req)
	secretMap := make(map[string][]byte)
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list secrets: %w", err)
		}
		key := sm.trimName(resp.Name)
		if ref.Path != nil && !strings.HasPrefix(key, *ref.Path) {
			continue
		}
		log.V(1).Info("gcp sm findByTags matches tags", "name", resp.Name)
		secretMap[key], err = sm.getData(ctx, key)
		if err != nil {
			return nil, err
		}
	}

	return utils.ConvertKeys(ref.ConversionStrategy, secretMap)
}

func (sm *ProviderGCP) trimName(name string) string {
	ProjectIDNumuber := sm.extractProjectIDNumber(name)
	key := strings.TrimPrefix(name, fmt.Sprintf("projects/%s/secrets/", ProjectIDNumuber))
	return key
}

// extractProjectIDNumber grabs the project id from the full name returned by gcp api
// gcp api seems to always return the number and not the project name
// (and users would always use the name, while requests accept both).
func (sm *ProviderGCP) extractProjectIDNumber(secretFullName string) string {
	s := strings.Split(secretFullName, "/")
	ProjectIDNumuber := s[1]
	return ProjectIDNumuber
}

// GetSecret returns a single secret from the provider.
func (sm *ProviderGCP) GetSecret(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	if utils.IsNil(sm.SecretManagerClient) || sm.ProjectID == "" {
		return nil, fmt.Errorf(errUninitalizedGCPProvider)
	}

	version := ref.Version
	if version == "" {
		version = defaultVersion
	}

	req := &secretmanagerpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/%s", sm.ProjectID, ref.Key, version),
	}
	result, err := sm.SecretManagerClient.AccessSecretVersion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf(errClientGetSecretAccess, err)
	}

	if ref.Property == "" {
		if result.Payload.Data != nil {
			return result.Payload.Data, nil
		}
		return nil, fmt.Errorf("invalid secret received. no secret string for key: %s", ref.Key)
	}

	var payload string
	if result.Payload.Data != nil {
		payload = string(result.Payload.Data)
	}
	idx := strings.Index(ref.Property, ".")
	refProperty := ref.Property
	if idx > 0 {
		refProperty = strings.ReplaceAll(refProperty, ".", "\\.")
		val := gjson.Get(payload, refProperty)
		if val.Exists() {
			return []byte(val.String()), nil
		}
	}
	val := gjson.Get(payload, ref.Property)
	if !val.Exists() {
		return nil, fmt.Errorf("key %s does not exist in secret %s", ref.Property, ref.Key)
	}
	return []byte(val.String()), nil
}

// GetSecretMap returns multiple k/v pairs from the provider.
func (sm *ProviderGCP) GetSecretMap(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	if sm.SecretManagerClient == nil || sm.ProjectID == "" {
		return nil, fmt.Errorf(errUninitalizedGCPProvider)
	}

	data, err := sm.GetSecret(ctx, ref)
	if err != nil {
		return nil, err
	}

	kv := make(map[string]json.RawMessage)
	err = json.Unmarshal(data, &kv)
	if err != nil {
		return nil, fmt.Errorf(errJSONSecretUnmarshal, err)
	}

	secretData := make(map[string][]byte)
	for k, v := range kv {
		var strVal string
		err = json.Unmarshal(v, &strVal)
		if err == nil {
			secretData[k] = []byte(strVal)
		} else {
			secretData[k] = v
		}
	}

	return secretData, nil
}

func (sm *ProviderGCP) Close(ctx context.Context) error {
	err := sm.SecretManagerClient.Close()
	if sm.gClient != nil {
		err = sm.gClient.Close()
	}
	useMu.Unlock()
	if err != nil {
		return fmt.Errorf(errClientClose, err)
	}
	return nil
}

func (sm *ProviderGCP) Validate() (esv1beta1.ValidationResult, error) {
	return esv1beta1.ValidationResultReady, nil
}

func (sm *ProviderGCP) ValidateStore(store esv1beta1.GenericStore) error {
	if store == nil {
		return fmt.Errorf(errInvalidStore)
	}
	spc := store.GetSpec()
	if spc == nil {
		return fmt.Errorf(errInvalidStoreSpec)
	}
	if spc.Provider == nil {
		return fmt.Errorf(errInvalidStoreProv)
	}
	p := spc.Provider.GCPSM
	if p == nil {
		return fmt.Errorf(errInvalidGCPProv)
	}
	if p.Auth.SecretRef != nil {
		if err := utils.ValidateSecretSelector(store, p.Auth.SecretRef.SecretAccessKey); err != nil {
			return fmt.Errorf(errInvalidAuthSecretRef, err)
		}
	}
	if p.Auth.WorkloadIdentity != nil {
		if err := utils.ValidateServiceAccountSelector(store, p.Auth.WorkloadIdentity.ServiceAccountRef); err != nil {
			return fmt.Errorf(errInvalidWISARef, err)
		}
	}
	return nil
}

func init() {
	esv1beta1.Register(&ProviderGCP{}, &esv1beta1.SecretStoreProvider{
		GCPSM: &esv1beta1.GCPSMProvider{},
	})
}
