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

package acr

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/go-autorest/autorest/azure"
	"k8s.io/client-go/kubernetes"
	kcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	genv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	smmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
	"github.com/external-secrets/external-secrets/pkg/provider/azure/keyvault"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Generator struct{}

type TokenGetter interface {
	GetToken(ctx context.Context, opts policy.TokenRequestOptions) (*azcore.AccessToken, error)
}

const (
	errNoSpec     = "no config spec provided"
	errParseSpec  = "unable to parse spec: %w"
	errCreateSess = "unable to create aws session: %w"
	errGetToken   = "unable to get authorization token: %w"
)

func (g *Generator) Generate(ctx context.Context, jsonSpec *apiextensions.JSON, crClient client.Client, namespace string) (map[string][]byte, error) {
	if jsonSpec == nil {
		return nil, fmt.Errorf(errNoSpec)
	}
	res, err := parseSpec(jsonSpec.Raw)
	if err != nil {
		return nil, fmt.Errorf(errParseSpec, err)
	}

	cfg, err := ctrlcfg.GetConfig()
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	var accessToken string
	// handle service principal
	if res.Spec.Auth.ServicePrincipal != nil {
		accessToken, err = accessTokenForServicePrincipal(
			ctx,
			crClient,
			namespace,
			res.Spec.EnvironmentType,
			res.Spec.TenantID,
			res.Spec.Auth.ServicePrincipal.SecretRef.ClientID,
			res.Spec.Auth.ServicePrincipal.SecretRef.ClientSecret,
		)
	} else if res.Spec.Auth.ManagedIdentity != nil {
		accessToken, err = accessTokenForManagedIdentity(
			ctx,
			res.Spec.EnvironmentType,
			res.Spec.Auth.ManagedIdentity.IdentityID,
		)
	} else if res.Spec.Auth.WorkloadIdentity != nil {
		accessToken, err = accessTokenForWorkloadIdentity(
			ctx,
			crClient,
			kubeClient.CoreV1(),
			res.Spec.ACRRegistry,
			res.Spec.EnvironmentType,
			res.Spec.Auth.WorkloadIdentity.ServiceAccountRef,
			namespace,
		)
	}
	if err != nil {
		return nil, err
	}
	fmt.Printf("aad access token: %s\n", accessToken)
	var acrToken string
	acrToken, err = fetchACRRefreshToken(accessToken, res.Spec.TenantID, res.Spec.ACRRegistry)
	if err != nil {
		return nil, err
	}
	if res.Spec.Scope != "" {
		acrToken, err = fetchACRAccessToken(acrToken, res.Spec.TenantID, res.Spec.ACRRegistry, res.Spec.Scope)
		if err != nil {
			return nil, err
		}
	}

	return map[string][]byte{
		"user":  []byte(`00000000-0000-0000-0000-000000000000`),
		"token": []byte(acrToken),
	}, nil
}

func fetchACRAccessToken(acrRefreshToken, tenantID, registryURL, scope string) (string, error) {
	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"service":       {registryURL},
		"scope":         {scope},
		"refresh_token": {acrRefreshToken},
	}
	res, err := http.PostForm(fmt.Sprintf("https://%s/oauth2/token", registryURL), formData)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", res.StatusCode)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	var payload map[string]string
	err = json.Unmarshal(body, &payload)
	if err != nil {
		return "", err
	}
	accessToken, ok := payload["access_token"]
	if !ok {
		return "", fmt.Errorf("unable to get token")
	}
	return accessToken, nil
}

func fetchACRRefreshToken(aadAccessToken, tenantID, registryURL string) (string, error) {
	// https://github.com/Azure/acr/blob/main/docs/AAD-OAuth.md#overview
	// https://docs.microsoft.com/en-us/azure/container-registry/container-registry-authentication?tabs=azure-cli
	formData := url.Values{
		"grant_type":   {"access_token"},
		"service":      {registryURL},
		"tenant":       {tenantID},
		"access_token": {aadAccessToken},
	}
	res, err := http.PostForm(fmt.Sprintf("https://%s/oauth2/exchange", registryURL), formData)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code %d, expected %d", res.StatusCode, http.StatusOK)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	var payload map[string]string
	err = json.Unmarshal(body, &payload)
	if err != nil {
		return "", err
	}
	refreshToken, ok := payload["refresh_token"]
	if !ok {
		return "", fmt.Errorf("unable to get token")
	}
	return refreshToken, nil
}

func accessTokenForWorkloadIdentity(ctx context.Context, crClient client.Client, kubeClient kcorev1.CoreV1Interface, acrRegistry string, envType v1beta1.AzureEnvironmentType, serviceAccountRef *smmeta.ServiceAccountSelector, namespace string) (string, error) {
	aadEndpoint := keyvault.AadEndpointForType(envType)
	if !strings.HasSuffix(acrRegistry, "/") {
		acrRegistry += "/"
	}
	acrResource := fmt.Sprintf("https://%s/.default", acrRegistry)
	// if no serviceAccountRef was provided
	// we expect certain env vars to be present.
	// They are set by the azure workload identity webhook.
	if serviceAccountRef == nil {
		clientID := os.Getenv("AZURE_CLIENT_ID")
		tenantID := os.Getenv("AZURE_TENANT_ID")
		tokenFilePath := os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
		if clientID == "" || tenantID == "" || tokenFilePath == "" {
			return "", errors.New("missing environment variables")
		}
		token, err := os.ReadFile(tokenFilePath)
		if err != nil {
			return "", fmt.Errorf("unable to read token file %s: %w", tokenFilePath, err)
		}
		tp, err := keyvault.NewTokenProvider(ctx, string(token), clientID, tenantID, aadEndpoint, acrResource)
		if err != nil {
			return "", err
		}
		return tp.OAuthToken(), nil
	}
	var sa corev1.ServiceAccount
	err := crClient.Get(ctx, types.NamespacedName{
		Name:      serviceAccountRef.Name,
		Namespace: namespace,
	}, &sa)
	if err != nil {
		return "", err
	}
	clientID, ok := sa.ObjectMeta.Annotations[keyvault.AnnotationClientID]
	if !ok {
		return "", fmt.Errorf("service account is missing annoation: %s", keyvault.AnnotationClientID)
	}
	tenantID, ok := sa.ObjectMeta.Annotations[keyvault.AnnotationTenantID]
	if !ok {
		return "", fmt.Errorf("service account is missing annotation: %s", keyvault.AnnotationTenantID)
	}
	audiences := []string{keyvault.AzureDefaultAudience}
	if len(serviceAccountRef.Audiences) > 0 {
		audiences = append(audiences, serviceAccountRef.Audiences...)
	}
	token, err := keyvault.FetchSAToken(ctx, namespace, serviceAccountRef.Name, audiences, kubeClient)
	if err != nil {
		return "", err
	}
	tp, err := keyvault.NewTokenProvider(ctx, token, clientID, tenantID, aadEndpoint, acrResource)
	if err != nil {
		return "", err
	}
	return tp.OAuthToken(), nil
}

func accessTokenForManagedIdentity(ctx context.Context, envType v1beta1.AzureEnvironmentType, identityID string) (string, error) {
	// handle workload identity
	creds, err := azidentity.NewManagedIdentityCredential(
		&azidentity.ManagedIdentityCredentialOptions{
			ID: azidentity.ResourceID(identityID),
		},
	)
	if err != nil {
		return "", err
	}
	aud := audienceForType(envType)
	accessToken, err := creds.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{aud},
	})
	if err != nil {
		return "", err
	}
	return accessToken.Token, nil
}

func accessTokenForServicePrincipal(ctx context.Context, crClient client.Client, namespace string, envType v1beta1.AzureEnvironmentType, tenantID string, idRef, secretRef smmeta.SecretKeySelector) (string, error) {
	cid, err := secretKeyRef(ctx, crClient, namespace, idRef)
	if err != nil {
		return "", err
	}
	csec, err := secretKeyRef(ctx, crClient, namespace, secretRef)
	if err != nil {
		return "", err
	}
	aadEndpoint := keyvault.AadEndpointForType(envType)
	creds, err := azidentity.NewClientSecretCredential(
		tenantID,
		cid,
		csec,
		&azidentity.ClientSecretCredentialOptions{
			AuthorityHost: azidentity.AuthorityHost(aadEndpoint),
		})
	if err != nil {
		return "", err
	}
	aud := audienceForType(envType)
	accessToken, err := creds.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{aud},
	})
	if err != nil {
		return "", err
	}
	return accessToken.Token, nil
}

// secretKeyRef fetches a secret key.
func secretKeyRef(ctx context.Context, crClient client.Client, namespace string, secretRef smmeta.SecretKeySelector) (string, error) {
	var secret corev1.Secret
	ref := types.NamespacedName{
		Namespace: namespace,
		Name:      secretRef.Name,
	}
	err := crClient.Get(ctx, ref, &secret)
	if err != nil {
		return "", fmt.Errorf("unable to find namespace=%q secret=%q %w", ref.Namespace, ref.Name, err)
	}
	keyBytes, ok := secret.Data[secretRef.Key]
	if !ok {
		return "", fmt.Errorf("unable to find key=%q secret=%q namespace=%q", secretRef.Key, secretRef.Name, namespace)
	}
	value := strings.TrimSpace(string(keyBytes))
	return value, nil
}

func audienceForType(t v1beta1.AzureEnvironmentType) string {
	suffix := ".default"
	switch t {
	case v1beta1.AzureEnvironmentChinaCloud:
		return azure.ChinaCloud.TokenAudience + suffix
	case v1beta1.AzureEnvironmentGermanCloud:
		return azure.GermanCloud.TokenAudience + suffix
	case v1beta1.AzureEnvironmentUSGovernmentCloud:
		return azure.USGovernmentCloud.TokenAudience + suffix
	}
	return azure.PublicCloud.TokenAudience + suffix
}

func parseSpec(data []byte) (*genv1alpha1.ACRAccessToken, error) {
	var spec genv1alpha1.ACRAccessToken
	err := json.Unmarshal(data, &spec)
	return &spec, err
}

func init() {
	genv1alpha1.Register(genv1alpha1.ACRAccessTokenKind, &Generator{})
}
