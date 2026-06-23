package azkv // import "github.com/getsops/sops/v3/azkv"

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	azidentitycache "github.com/Azure/azure-sdk-for-go/sdk/azidentity/cache"
	"github.com/pkg/browser"
)

const (
	SopsAzureAuthMethodEnv             = "SOPS_AZURE_AUTH_METHOD"
	cachedBrowserAuthRecordFileName    = "azure-auth-record-browser.json"
	cachedDeviceCodeAuthRecordFileName = "azure-auth-record-device-code.json"
)

func sopsCacheDir() (string, error) {
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	cacheDir := filepath.Join(userCacheDir, "/sops")

	if err = os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}

	return cacheDir, nil
}

type CachableTokenCredential interface {
	Authenticate(ctx context.Context, opts *policy.TokenRequestOptions) (azidentity.AuthenticationRecord, error)
	GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error)
}

func cacheStoreRecord(cachePath string, record azidentity.AuthenticationRecord) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}

	return os.WriteFile(cachePath, b, 0600)
}

func cacheLoadRecord(cachePath string) (azidentity.AuthenticationRecord, error) {
	var record azidentity.AuthenticationRecord

	b, err := os.ReadFile(cachePath)
	if err != nil {
		return record, err
	}

	err = json.Unmarshal(b, &record)
	if err != nil {
		return record, err
	}

	return record, nil
}

func cacheTokenCredential(cachePath string, tokenCredentialFn func(cache azidentity.Cache, record azidentity.AuthenticationRecord) (CachableTokenCredential, error)) (azcore.TokenCredential, error) {
	cache, err := azidentitycache.New(nil)
	// Errors if persistent caching is not supported by the current runtime
	if err != nil {
		return nil, err
	}

	cachedRecord, cacheLoadErr := cacheLoadRecord(cachePath)

	credential, err := tokenCredentialFn(cache, cachedRecord)
	if err != nil {
		return nil, err
	}

	// If loading the authenticationRecord from the cachePath failed for any reason (validation, file doesn't exist, not encoded using json, etc.)
	if cacheLoadErr != nil {
		record, err := credential.Authenticate(context.Background(), nil)
		if err != nil {
			return nil, err
		}

		if err = cacheStoreRecord(cachePath, record); err != nil {
			return nil, err
		}
	}

	return credential, nil
}

func cachedInteractiveBrowserCredentials() (azcore.TokenCredential, error) {
	// The default behaviour of `browser` which `azidentity` is using for the interactive browser authentication method is to write anything the browser prints to stdout to the stdout of the program running it.
	// This is not desired since on the initial authentication or when refreshing the cache it would pollute the output of sops.
	// To fix this behaviour we redirect the browser stdout -> stderr so any pertinent information written by the browser is not completely hidden from the user but it doesn't mess up the sops output.
	browser.Stdout = os.Stderr

	cacheDir, err := sopsCacheDir()
	if err != nil {
		return nil, err
	}

	return cacheTokenCredential(
		filepath.Join(cacheDir, cachedBrowserAuthRecordFileName),
		func(cache azidentity.Cache, record azidentity.AuthenticationRecord) (CachableTokenCredential, error) {
			return azidentity.NewInteractiveBrowserCredential(&azidentity.InteractiveBrowserCredentialOptions{
				AuthenticationRecord: record,
				Cache:                cache,
			})
		},
	)
}

func cachedDeviceCodeCredentials() (azcore.TokenCredential, error) {
	cacheDir, err := sopsCacheDir()
	if err != nil {
		return nil, err
	}

	return cacheTokenCredential(
		filepath.Join(cacheDir, cachedDeviceCodeAuthRecordFileName),
		func(cache azidentity.Cache, record azidentity.AuthenticationRecord) (CachableTokenCredential, error) {
			return azidentity.NewDeviceCodeCredential(&azidentity.DeviceCodeCredentialOptions{
				AuthenticationRecord: record,
				Cache:                cache,
				// Print the device code authentication information to stderr so we don't pollute the output of sops.
				UserPrompt: func(ctx context.Context, dc azidentity.DeviceCodeMessage) error {
					_, err := fmt.Fprintln(os.Stderr, dc.Message)
					return err

				},
			})
		},
	)
}
