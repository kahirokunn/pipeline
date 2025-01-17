//
// Copyright 2022 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package azure implement the interface with microsoft azure kms service
package azure

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v3"
	"github.com/jellydator/ttlcache/v3"

	kvauth "github.com/Azure/azure-sdk-for-go/services/keyvault/auth"
	"github.com/Azure/azure-sdk-for-go/services/keyvault/v7.1/keyvault"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sigstore/sigstore/pkg/signature"
	sigkms "github.com/sigstore/sigstore/pkg/signature/kms"
)

func init() {
	sigkms.AddProvider(ReferenceScheme, func(ctx context.Context, keyResourceID string, hashFunc crypto.Hash, opts ...signature.RPCOption) (sigkms.SignerVerifier, error) {
		return LoadSignerVerifier(ctx, keyResourceID, hashFunc)
	})
}

type kvClient interface {
	CreateKey(ctx context.Context, vaultBaseURL, keyName string, parameters keyvault.KeyCreateParameters) (result keyvault.KeyBundle, err error)
	GetKey(ctx context.Context, vaultBaseURL, keyName, keyVersion string) (result keyvault.KeyBundle, err error)
	Sign(ctx context.Context, vaultBaseURL, keyName, keyVersion string, parameters keyvault.KeySignParameters) (result keyvault.KeyOperationResult, err error)
	Verify(ctx context.Context, vaultBaseURL, keyName, keyVersion string, parameters keyvault.KeyVerifyParameters) (result keyvault.KeyVerifyResult, err error)
}

type azureVaultClient struct {
	client    kvClient
	keyCache  *ttlcache.Cache[string, crypto.PublicKey]
	vaultURL  string
	vaultName string
	keyName   string
}

var (
	errAzureReference = errors.New("kms specification should be in the format azurekms://[VAULT_NAME][VAULT_URL]/[KEY_NAME]")

	referenceRegex = regexp.MustCompile(`^azurekms://([^/]+)/([^/]+)?$`)
)

const (
	// ReferenceScheme schemes for various KMS services are copied from https://github.com/google/go-cloud/tree/master/secrets
	ReferenceScheme = "azurekms://"
	cacheKey        = "azure_vault_signer"
)

// ValidReference returns a non-nil error if the reference string is invalid
func ValidReference(ref string) error {
	if !referenceRegex.MatchString(ref) {
		return errAzureReference
	}
	return nil
}

func parseReference(resourceID string) (vaultURL, vaultName, keyName string, err error) {
	v := referenceRegex.FindStringSubmatch(resourceID)
	if len(v) != 3 {
		err = fmt.Errorf("invalid azurekms format %q", resourceID)
		return
	}

	vaultURL = fmt.Sprintf("https://%s/", v[1])
	vaultName, keyName = strings.Split(v[1], ".")[0], v[2]
	return
}

func newAzureKMS(_ context.Context, keyResourceID string) (*azureVaultClient, error) {
	if err := ValidReference(keyResourceID); err != nil {
		return nil, err
	}
	vaultURL, vaultName, keyName, err := parseReference(keyResourceID)
	if err != nil {
		return nil, err
	}

	client, err := getKeysClient()
	if err != nil {
		return nil, fmt.Errorf("new azure kms client: %w", err)
	}

	azClient := &azureVaultClient{
		client:    &client,
		vaultURL:  vaultURL,
		vaultName: vaultName,
		keyName:   keyName,
		keyCache: ttlcache.New[string, crypto.PublicKey](
			ttlcache.WithDisableTouchOnHit[string, crypto.PublicKey](),
		),
	}

	return azClient, nil
}

type authenticationMethod string

const (
	unknownAuthenticationMethod     = "unknown"
	environmentAuthenticationMethod = "environment"
	cliAuthenticationMethod         = "cli"
)

// getAuthMethod returns the an authenticationMethod to use to get an Azure Authorizer.
// If no environment variables are set, unknownAuthMethod will be used.
// If the environment variable 'AZURE_AUTH_METHOD' is set to either environment or cli, use it.
// If the environment variables 'AZURE_TENANT_ID', 'AZURE_CLIENT_ID' and 'AZURE_CLIENT_SECRET' are set, use environment.
func getAuthenticationMethod() authenticationMethod {
	tenantID := os.Getenv("AZURE_TENANT_ID")
	clientID := os.Getenv("AZURE_CLIENT_ID")
	clientSecret := os.Getenv("AZURE_CLIENT_SECRET")
	authMethod := os.Getenv("AZURE_AUTH_METHOD")

	if authMethod != "" {
		switch strings.ToLower(authMethod) {
		case "environment":
			return environmentAuthenticationMethod
		case "cli":
			return cliAuthenticationMethod
		}
	}

	if tenantID != "" && clientID != "" && clientSecret != "" {
		return environmentAuthenticationMethod
	}

	return unknownAuthenticationMethod
}

// getAuthorizer takes an authenticationMethod and returns an Authorizer or an error.
// If the method is unknown, Environment will be tested and if it returns an error CLI will be tested.
// If the method is specified, the specified method will be used and no other will be tested.
// This means the following default order of methods will be used if nothing else is defined:
// 1. Client credentials (FromEnvironment)
// 2. Client certificate (FromEnvironment)
// 3. Username password (FromEnvironment)
// 4. MSI (FromEnvironment)
// 5. CLI (FromCLI)
func getAuthorizer(method authenticationMethod) (autorest.Authorizer, error) {
	switch method {
	case environmentAuthenticationMethod:
		return kvauth.NewAuthorizerFromEnvironment()
	case cliAuthenticationMethod:
		return kvauth.NewAuthorizerFromCLI()
	case unknownAuthenticationMethod:
		break
	default:
		return nil, fmt.Errorf("you should never reach this")
	}

	authorizer, err := kvauth.NewAuthorizerFromEnvironment()
	if err == nil {
		return authorizer, nil
	}

	return kvauth.NewAuthorizerFromCLI()
}

func getKeysClient() (keyvault.BaseClient, error) {
	keyClient := keyvault.New()

	authMethod := getAuthenticationMethod()
	authorizer, err := getAuthorizer(authMethod)
	if err != nil {
		return keyvault.BaseClient{}, err
	}

	keyClient.Authorizer = authorizer
	err = keyClient.AddToUserAgent("sigstore")
	if err != nil {
		return keyvault.BaseClient{}, err
	}

	return keyClient, nil
}

func (a *azureVaultClient) fetchPublicKey(ctx context.Context) (crypto.PublicKey, error) {
	keyBundle, err := a.getKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}

	key := keyBundle.Key
	keyType := string(key.Kty)

	// Azure Key Vault allows keys to be stored in either default Key Vault storage
	// or in managed HSMs. If the key is stored in a HSM, the key type is suffixed
	// with "-HSM". Since this suffix is specific to Azure Key Vault, it needs
	// be stripped from the key type before attempting to represent the key
	// with a go-jose/JSONWebKey struct.
	if strings.HasSuffix(keyType, "-HSM") {
		split := strings.Split(keyType, "-HSM")
		// since we split on the suffix, there should be only two elements
		// the first element should contain the key type without the -HSM suffix
		newKeyType := split[0]
		key.Kty = keyvault.JSONWebKeyType(newKeyType)
	}

	jwkJSON, err := json.Marshal(*key)
	if err != nil {
		return nil, fmt.Errorf("encoding the jsonWebKey: %w", err)
	}

	jwk := jose.JSONWebKey{}
	err = jwk.UnmarshalJSON(jwkJSON)
	if err != nil {
		return nil, fmt.Errorf("decoding the jsonWebKey: %w", err)
	}

	pub, ok := jwk.Key.(*ecdsa.PublicKey)
	if !ok {
		if err != nil {
			return nil, fmt.Errorf("public key was not ECDSA: %#v", pub)
		}
	}

	return pub, nil
}

func (a *azureVaultClient) getKey(ctx context.Context) (keyvault.KeyBundle, error) {
	key, err := a.client.GetKey(ctx, a.vaultURL, a.keyName, "")
	if err != nil {
		return keyvault.KeyBundle{}, fmt.Errorf("public key: %w", err)
	}

	return key, err
}

func (a *azureVaultClient) public(ctx context.Context) (crypto.PublicKey, error) {
	var lerr error
	loader := ttlcache.LoaderFunc[string, crypto.PublicKey](
		func(c *ttlcache.Cache[string, crypto.PublicKey], key string) *ttlcache.Item[string, crypto.PublicKey] {
			ttl := 300 * time.Second
			var pubKey crypto.PublicKey
			pubKey, lerr = a.fetchPublicKey(ctx)
			if lerr == nil {
				return c.Set(cacheKey, pubKey, ttl)
			}
			return nil
		},
	)
	item := a.keyCache.Get(cacheKey, ttlcache.WithLoader[string, crypto.PublicKey](loader))
	if lerr != nil {
		return nil, lerr
	}
	return item.Value(), nil
}

func (a *azureVaultClient) createKey(ctx context.Context) (crypto.PublicKey, error) {
	_, err := a.getKey(ctx)
	if err == nil {
		return a.public(ctx)
	}

	_, err = a.client.CreateKey(
		ctx,
		a.vaultURL,
		a.keyName,
		keyvault.KeyCreateParameters{
			KeyAttributes: &keyvault.KeyAttributes{
				Enabled: to.BoolPtr(true),
			},
			KeySize: to.Int32Ptr(2048),
			KeyOps: &[]keyvault.JSONWebKeyOperation{
				keyvault.Sign,
				keyvault.Verify,
			},
			Kty: keyvault.EC,
			Tags: map[string]*string{
				"use": to.StringPtr("sigstore"),
			},
		})
	if err != nil {
		return nil, err
	}

	return a.public(ctx)
}

func getKeyVaultSignatureAlgo(algo crypto.Hash) (keyvault.JSONWebKeySignatureAlgorithm, error) {
	switch algo {
	case crypto.SHA256:
		return keyvault.ES256, nil
	case crypto.SHA384:
		return keyvault.ES384, nil
	case crypto.SHA512:
		return keyvault.ES512, nil
	default:
		return "", fmt.Errorf("unsupported algorithm: %s", algo)
	}
}

func (a *azureVaultClient) sign(ctx context.Context, hash []byte, algo crypto.Hash) ([]byte, error) {
	keyVaultAlgo, err := getKeyVaultSignatureAlgo(algo)
	if err != nil {
		return nil, fmt.Errorf("failed to get KeyVaultSignatureAlgorithm: %w", err)
	}

	params := keyvault.KeySignParameters{
		Algorithm: keyVaultAlgo,
		Value:     to.StringPtr(base64.RawURLEncoding.EncodeToString(hash)),
	}

	result, err := a.client.Sign(ctx, a.vaultURL, a.keyName, "", params)
	if err != nil {
		return nil, fmt.Errorf("signing the payload: %w", err)
	}

	decResult, err := base64.RawURLEncoding.DecodeString(*result.Result)
	if err != nil {
		return nil, fmt.Errorf("decoding the result: %w", err)
	}

	return decResult, nil
}

func (a *azureVaultClient) verify(ctx context.Context, signature, hash []byte, algo crypto.Hash) error {
	keyVaultAlgo, err := getKeyVaultSignatureAlgo(algo)
	if err != nil {
		return fmt.Errorf("failed to get KeyVaultSignatureAlgorithm: %w", err)
	}

	params := keyvault.KeyVerifyParameters{
		Algorithm: keyVaultAlgo,
		Digest:    to.StringPtr(base64.RawURLEncoding.EncodeToString(hash)),
		Signature: to.StringPtr(base64.RawURLEncoding.EncodeToString(signature)),
	}

	result, err := a.client.Verify(ctx, a.vaultURL, a.keyName, "", params)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	if !*result.Value {
		return errors.New("failed vault verification")
	}

	return nil
}
