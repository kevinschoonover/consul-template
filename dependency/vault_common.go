package dependency

import (
	"log"
	"math/rand"
	"path"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
)

var (
	// VaultDefaultLeaseDuration is the default lease duration in seconds.
	VaultDefaultLeaseDuration = 5 * time.Minute
)

// Secret is the structure returned for every secret within Vault.
type Secret struct {
	// The request ID that generated this response
	RequestID string

	LeaseID       string
	LeaseDuration int
	Renewable     bool

	// Data is the actual contents of the secret. The format of the data
	// is arbitrary and up to the secret backend.
	Data map[string]interface{}

	// Warnings contains any warnings related to the operation. These
	// are not issues that caused the command to fail, but that the
	// client should be aware of.
	Warnings []string

	// Auth, if non-nil, means that there was authentication information
	// attached to this response.
	Auth *SecretAuth

	// WrapInfo, if non-nil, means that the initial response was wrapped in the
	// cubbyhole of the given token (which has a TTL of the given number of
	// seconds)
	WrapInfo *SecretWrapInfo
}

// SecretAuth is the structure containing auth information if we have it.
type SecretAuth struct {
	ClientToken string
	Accessor    string
	Policies    []string
	Metadata    map[string]string

	LeaseDuration int
	Renewable     bool
}

// SecretWrapInfo contains wrapping information if we have it. If what is
// contained is an authentication token, the accessor for the token will be
// available in WrappedAccessor.
type SecretWrapInfo struct {
	Token           string
	TTL             int
	CreationTime    time.Time
	WrappedAccessor string
}

// vaultRenewDuration accepts a secret and returns the recommended amount of
// time to sleep.
func vaultRenewDuration(s *Secret) time.Duration {
	// Handle whether this is an auth or a regular secret.
	base := s.LeaseDuration
	if s.Auth != nil && s.Auth.LeaseDuration > 0 {
		base = s.Auth.LeaseDuration
	}

	// Ensure we have a lease duration, since sometimes this can be zero.
	if base <= 0 {
		base = int(VaultDefaultLeaseDuration.Seconds())
	}

	// Convert to float seconds.
	sleep := float64(time.Duration(base) * time.Second)

	if vaultSecretRenewable(s) {
		// Renew at 1/3 the remaining lease. This will give us an opportunity to retry
		// at least one more time should the first renewal fail.
		sleep = sleep / 3.0

		// Use a randomness so many clients do not hit Vault simultaneously.
		sleep = sleep * (rand.Float64() + 1) / 2.0
	} else {
		// For non-renewable leases set the renew duration to use much of the secret
		// lease as possible. Use a stagger over 85%-95% of the lease duration so that
		// many clients do not hit Vault simultaneously.
		sleep = sleep * (.85 + rand.Float64()*0.1)
	}

	return time.Duration(sleep)
}

// printVaultWarnings prints warnings for a given dependency.
func printVaultWarnings(d Dependency, warnings []string) {
	for _, w := range warnings {
		log.Printf("[WARN] %s: %s", d, w)
	}
}

// vaultSecretRenewable determines if the given secret is renewable.
func vaultSecretRenewable(s *Secret) bool {
	if s.Auth != nil {
		return s.Auth.Renewable
	}
	return s.Renewable
}

// transformSecret transforms an api secret into our secret. This does not deep
// copy underlying deep data structures, so it's not safe to modify the vault
// secret as that may modify the data in the transformed secret.
func transformSecret(theirs *api.Secret) *Secret {
	var ours Secret
	updateSecret(&ours, theirs)
	return &ours
}

// updateSecret updates our secret with the new data from the api, careful to
// not overwrite missing data. Renewals don't include the original secret, and
// we don't want to delete that data accidentally.
func updateSecret(ours *Secret, theirs *api.Secret) {
	if theirs.RequestID != "" {
		ours.RequestID = theirs.RequestID
	}

	if theirs.LeaseID != "" {
		ours.LeaseID = theirs.LeaseID
	}

	if theirs.LeaseDuration != 0 {
		ours.LeaseDuration = theirs.LeaseDuration
	}

	if theirs.Renewable {
		ours.Renewable = theirs.Renewable
	}

	if len(theirs.Data) != 0 {
		ours.Data = theirs.Data
	}

	if len(theirs.Warnings) != 0 {
		ours.Warnings = theirs.Warnings
	}

	if theirs.Auth != nil {
		if ours.Auth == nil {
			ours.Auth = &SecretAuth{}
		}

		if theirs.Auth.ClientToken != "" {
			ours.Auth.ClientToken = theirs.Auth.ClientToken
		}

		if theirs.Auth.Accessor != "" {
			ours.Auth.Accessor = theirs.Auth.Accessor
		}

		if len(theirs.Auth.Policies) != 0 {
			ours.Auth.Policies = theirs.Auth.Policies
		}

		if len(theirs.Auth.Metadata) != 0 {
			ours.Auth.Metadata = theirs.Auth.Metadata
		}

		if theirs.Auth.LeaseDuration != 0 {
			ours.Auth.LeaseDuration = theirs.Auth.LeaseDuration
		}

		if theirs.Auth.Renewable {
			ours.Auth.Renewable = theirs.Auth.Renewable
		}
	}

	if theirs.WrapInfo != nil {
		if ours.WrapInfo == nil {
			ours.WrapInfo = &SecretWrapInfo{}
		}

		if theirs.WrapInfo.Token != "" {
			ours.WrapInfo.Token = theirs.WrapInfo.Token
		}

		if theirs.WrapInfo.TTL != 0 {
			ours.WrapInfo.TTL = theirs.WrapInfo.TTL
		}

		if !theirs.WrapInfo.CreationTime.IsZero() {
			ours.WrapInfo.CreationTime = theirs.WrapInfo.CreationTime
		}

		if theirs.WrapInfo.WrappedAccessor != "" {
			ours.WrapInfo.WrappedAccessor = theirs.WrapInfo.WrappedAccessor
		}
	}
}

func isKVv2(client *api.Client, path string) (string, bool, error) {
	// We don't want to use a wrapping call here so save any custom value and
	// restore after
	currentWrappingLookupFunc := client.CurrentWrappingLookupFunc()
	client.SetWrappingLookupFunc(nil)
	defer client.SetWrappingLookupFunc(currentWrappingLookupFunc)
	currentOutputCurlString := client.OutputCurlString()
	client.SetOutputCurlString(false)
	defer client.SetOutputCurlString(currentOutputCurlString)

	r := client.NewRequest("GET", "/v1/sys/internal/ui/mounts/"+path)
	resp, err := client.RawRequest(r)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		// If we get a 404 we are using an older version of vault, default to
		// version 1
		if resp != nil && resp.StatusCode == 404 {
			return "", false, nil
		}

		return "", false, err
	}

	secret, err := api.ParseSecret(resp.Body)
	if err != nil {
		return "", false, err
	}
	var mountPath string
	if mountPathRaw, ok := secret.Data["path"]; ok {
		mountPath = mountPathRaw.(string)
	}
	var mountType string
	if mountTypeRaw, ok := secret.Data["type"]; ok {
		mountType = mountTypeRaw.(string)
	}
	options := secret.Data["options"]
	if options == nil {
		return mountPath, false, nil
	}
	versionRaw := options.(map[string]interface{})["version"]
	if versionRaw == nil {
		return mountPath, false, nil
	}
	version := versionRaw.(string)
	switch version {
	case "", "1":
		return mountPath, false, nil
	case "2":
		return mountPath, mountType == "kv", nil
	}

	return mountPath, false, nil
}

func addPrefixToVKVPath(p, mountPath, apiPrefix string) string {
	switch {
	case p == mountPath, p == strings.TrimSuffix(mountPath, "/"):
		return path.Join(mountPath, apiPrefix)
	default:
		p = strings.TrimPrefix(p, mountPath)
		return path.Join(mountPath, apiPrefix, p)
	}
}
