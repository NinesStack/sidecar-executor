// vault handles the Hashicorp Vault secret store. It uses the default Vault
// environment variables for configuration and adds a couple more. If you
// supply a token by some means, it will use that. If not, it will either fetch
// a token from a specified file, or fall back to userpass auth.
//
// You should provide at least the following:
//
//  * VAULT_ADDR - URL of the Vault server
//  * VAULT_MAX_RETRIES - API retries before Vault fails
//  * VAULT_TOKEN - Optional if specified in a file or using userpass
//  * VAULT_TOKEN_FILE - Where to cache Vault tokens between calls to the
//    executor on the same host.
//  * VAULT_TTL - The TTL in seconds of the Vault Token we'll have issued
//    note that the grace period is one hour so shorter than 1 hour is not
//    possible.
package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	VaultURLScheme  = "vault"
	VaultDefaultKey = "value"

	DefaultAWSRoleTTL = 3600 // 1 hour
)

// The Vault interface represents a client that talks to Hashicorp Vault and
// does some lower level work on our behalf
type Vault interface {
	DecryptAllEnv([]string) ([]string, error)
	GetAWSCredsLease(role string) (*VaultAWSCredsLease, error)
	RevokeAWSCredsLease(leaseID, role string) error
	RenewAWSCredsLease(awsCredsLease *VaultAWSCredsLease, ttl int) (*VaultAWSCredsLease, error)
	MaybeRevokeToken() error
}

// Client to replace vault paths by the secret value stored in Hashicorp Vault.
type EnvVault struct {
	client VaultAPI

	// The token we are using, *if* it's specific to this executor and not shared
	token string
}

// Our own narrowly-scoped interface for Hashicorp Vault Client
type VaultAPI interface {
	Address() string
	NewRequest(method, path string) *api.Request
	RawRequest(r *api.Request) (*api.Response, error)
}

// NewDefaultVault returns a client using the default configuration.
//
// The default Address is https://127.0.0.1:8200, but this can be overridden by
// setting the `VAULT_ADDR` environment variable.
func NewDefaultVault() EnvVault {
	conf := api.DefaultConfig()
	err := conf.ReadEnvironment()
	if err != nil {
		log.Warnf("Unable to load Environment vars: %s", err)
	}
	log.Infof("Vault address '%s' ", conf.Address)

	vaultClient, _ := api.NewClient(conf)
	envVault := EnvVault{client: vaultClient}

	// Check if we are supposed to have our own token. If so, get one. Otherwise
	// attempt to use the shared token from the file. If we're handling a role-specific
	// set of creds, we need our own token's TTL to match those of the request.
	if os.Getenv("EXECUTOR_AWS_ROLE") != "" {
		err := getAWSRoleVaultToken(&envVault)
		if err != nil {
			log.Errorf("Failed to get Vault parent Token to enable AWS Role: %s", err)
		}
	} else if os.Getenv("VAULT_TOKEN") == "" {
		// This will be a shared token
		err := GetToken(&vaultTokenAuthHandler{client: vaultClient})
		if err != nil {
			log.Errorf("Failure authenticating with Vault: %s", err)
		}
	}
	// otherwise the Vault client will use the one in the env var itself

	return envVault
}

// getAWSRoleVaultToken is called when we need to have a TTL and token to match
// those we'll be requesting for the service. Otherwise we can end up with the
// service token expiring before we expect that to happen.
func getAWSRoleVaultToken(envVault *EnvVault) error {
	var (
		ttl int = DefaultAWSRoleTTL
		err error
	)

	log.Info("Attempting to get a parent token with TTL to match requested AWS Role")

	if ttlStr := os.Getenv("EXECUTOR_AWS_TTL"); ttlStr != "" {
		ttl, err = parseTokenTTL(ttlStr)
		if err != nil {
			return err
		}
	}

	handler := &vaultTokenAuthHandler{client: envVault.client.(*api.Client)}

	token, err := GetTokenWithLogin(handler, ttl)
	if err != nil {
		return err
	}

	// Set the token on the client for use throughout its lifecycle
	handler.SetToken(token)
	envVault.token = token

	return nil
}

// parseTokenTTL parse a token duration and converts down to an integer in seconds
func parseTokenTTL(ttlStr string) (int, error) {
	ttlTmp, err := time.ParseDuration(ttlStr)
	if ttlTmp < 1 || err != nil {
		return -1, fmt.Errorf("Invalid TTL passed in Docker label vaul.AWSRoleTTL. Could not parse: '%s'", ttlStr)
	}

	// Seconds() returns a float64. We want the seconds, downgraded to an int
	ttl := int(ttlTmp.Seconds())

	return ttl, nil
}

// DecryptAllEnv decrypts all env vars that contain a Vault path.  All values
// staring with `vault://` are overridden by the secret value stored in the
// path. For instance:
//    Input: ["db_url=url","db_pass=vault://secret/db_pass"]
//   Output: ["db_url=url","db_pass=ACTUAL_SECRET_PASS"]
//
//
// By default, the key used to retrieve the contents of the Secret that Vault
// returns is the string `VaultefaultKey`. If you have more than one entry stored in a
// Secret and need to refer to them by name, you may append a query string
// specifying the key, such as:
//    vault://secret/prod-database?key=username
//
func (v EnvVault) DecryptAllEnv(envs []string) ([]string, error) {
	var decryptedEnv []string
	for _, env := range envs {
		keyValue := strings.SplitN(env, "=", 2)
		envName := keyValue[0]
		envValue := keyValue[1]

		if isVaultPath(envValue) {
			log.Debugf("Fetching secret value for path: '%s' [VAULT_ADDR: %s]", envValue, v.client.Address())
			decrypted, err := v.ReadSecretValue(envValue)
			if err != nil {
				return nil, err
			}
			log.Infof("Decrypted '%s' [VAULT_ADDR: %s, path: %s ]", envName, v.client.Address(), envValue)
			envValue = decrypted
		}
		decryptedEnv = append(decryptedEnv, fmt.Sprintf("%s=%s", envName, envValue))
	}
	return decryptedEnv, nil
}

// ReadSecretValue returns the secret value of a Vault path.
func (v EnvVault) ReadSecretValue(vaultURL string) (string, error) {
	parsed, err := url.Parse(vaultURL)
	if err != nil {
		return "", errors.Errorf("Unable to parse Vault URL: '%s'", vaultURL)
	}

	if parsed.Scheme != VaultURLScheme {
		return "", errors.Errorf("Invalid Vault URL '%s', expecting scheme '%s://'", vaultURL, VaultURLScheme)
	}

	path := parsed.Host + parsed.Path
	secret, err := v.read(path)

	if err != nil {
		return "", errors.Errorf("Unable to fetch '%s' [VAULT_ADDR:%s] error: %s", path, v.client.Address(), err)
	}

	if secret == nil {
		return "", errors.Errorf("Path '%s' not found [VAULT_ADDR:%s]", path, v.client.Address())
	}

	q := parsed.Query()
	key := q["key"]
	if key == nil {
		key = []string{VaultDefaultKey}
	}

	value, ok := secret.Data[key[0]].(string)

	if !ok {
		return "", errors.Errorf("Value for path '%s' not found [VAULT_ADDR:%s]", path, v.client.Address())
	}

	return value, nil
}

func isVaultPath(value string) bool {
	hasPrefix := strings.HasPrefix(value, VaultURLScheme+"://")
	return hasPrefix
}

// read secret using http Vault api
func (v EnvVault) read(path string) (*api.Secret, error) {
	r := v.client.NewRequest("GET", "/v1/"+path)
	resp, err := v.client.RawRequest(r)

	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil
	}

	return api.ParseSecret(resp.Body)
}

// AWS Role response from Vault:
// {
//    "request_id" : "933ebec3-213d-6ad5-e929-a20cd97ede43",
//    "data" : {
//       "secret_key" : "BnpDs61cbFauqBqc59qYjWIl0yOCsLsOHoNpHKUk",
//       "access_key" : "AKIAZQU2SSNZNDWB3NRL",
//       "security_token" : null
//    },
//    "lease_id" : "aws/creds/sidecar-executor-test-role/qE4IBWGAlWqExurMaKPdNSgG",
//    "warnings" : null,
//    "lease_duration" : 86400,
//    "renewable" : true,
//    "auth" : null,
//    "wrap_info" : null
// }

// A VaultAWSCredsResponse represents a response from the Vault API itself
// containing the AWS keys and tokens, etc.
type VaultAWSCredsResponse struct {
	RequestID string `json:"request_id"`
	Data      struct {
		SecretKey     string      `json:"secret_key"`
		AccessKey     string      `json:"access_key"`
		SecurityToken interface{} `json:"security_token"`
	} `json:"data"`
	LeaseID       string      `json:"lease_id"`
	Warnings      interface{} `json:"warnings"`
	LeaseDuration int         `json:"lease_duration"`
	Renewable     bool        `json:"renewable"`
	Auth          interface{} `json:"auth"`
	WrapInfo      interface{} `json:"wrap_info"`
}

// A VaultAWSCredsLease is returned from GetAWSCredsLease
type VaultAWSCredsLease struct {
	Vars            []string
	LeaseExpiryTime time.Time
	LeaseID         string
	Role            string
}

// MaybeRevokeToken will be called on shutdown, and *if* we cached a parent
// token that was specific to this service, then we will expire it. If we
// are using the shared token, we will not expire it.
func (v EnvVault) MaybeRevokeToken() error {
	// If we don't have one, this is a noop
	if v.token == "" {
		return nil
	}

	log.Infof("Revoking service-specific parent token in Vault: %s", v.token)

	r := v.client.NewRequest("POST", "/v1/auth/token/revoke-self")

	resp, err := v.client.RawRequest(r) // No body
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// The Vault API is a little bit shit. We get a 204 back if the request was
	// accepted, whether or not this is a valid lease. Adding the `sync` arg
	// does not change this behavior. So... 204 is all we can look at.
	if resp.StatusCode != 204 {
		return fmt.Errorf(
			"Failed to revoke service-specific parent token, got response code %d",
			resp.StatusCode,
		)
	}

	log.Info("Lease revoked")

	return nil
}

// GetAWSCredsLease calls the Vault API and asks for AWS creds for a particular role,
// returning a string slice of vars of the form "VAR=value" and/or an error if needed
func (v EnvVault) GetAWSCredsLease(role string) (*VaultAWSCredsLease, error) {
	r := v.client.NewRequest("GET", "/v1/aws/creds/"+role)

	resp, err := v.client.RawRequest(r)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Failed to get AWS creds lease, got response code %d", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to read Vault response body: %w", err)
	}

	var creds VaultAWSCredsResponse
	err = json.Unmarshal(data, &creds)
	if err != nil {
		return nil, fmt.Errorf("Unable to unmarshal Vault response body: %w", err)
	}

	// Set up a padded expiry time to make sure we never run over
	expiryTime := time.Now().UTC().Add(time.Duration(creds.LeaseDuration-360) * time.Second)

	// Construct the AWS env vars
	vars := []string{
		fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", creds.Data.AccessKey),
		fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", creds.Data.SecretKey),
	}

	return &VaultAWSCredsLease{
		Vars:            vars,
		LeaseExpiryTime: expiryTime,
		LeaseID:         creds.LeaseID,
		Role:            role,
	}, nil
}

// RevokeAWSCreds calls Vault and revokes an existing lease on AWS credentials
func (v EnvVault) RevokeAWSCredsLease(leaseID, role string) error {
	log.Infof("Revoking AWS lease ID '%s' for role '%s' in Vault", leaseID, role)

	r := v.client.NewRequest("PUT", "/v1/sys/leases/revoke")

	bodyStruct := struct {
		LeaseID string `json:"lease_id"`
	}{
		LeaseID: leaseID,
	}

	body, err := json.Marshal(bodyStruct)
	if err != nil {
		return fmt.Errorf("Unable to JSON encode lease revocation body: %w", err)
	}

	r.Body = bytes.NewBuffer(body)
	resp, err := v.client.RawRequest(r)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// The Vault API is a little bit shit. We get a 204 back if the request was
	// accepted, whether or not this is a valid lease. Adding the `sync` arg
	// does not change this behavior. So... 204 is all we can look at.
	if resp.StatusCode != 204 {
		return fmt.Errorf("Failed to revoke AWS creds lease, got response code %d", resp.StatusCode)
	}

	log.Info("Lease revoked")

	return nil
}

// RenewAWSCredsLease will renew the lease we already have on Vault, using the
// new TTL.  It can't return a fully populated lease but returns the values
// that have possibly changed.
func (v EnvVault) RenewAWSCredsLease(awsCredsLease *VaultAWSCredsLease, ttl int) (*VaultAWSCredsLease, error) {
	log.Infof("Renewing AWS lease ID '%s' for ttl '%d' in Vault", awsCredsLease.LeaseID, ttl)

	r := v.client.NewRequest("PUT", "/v1/sys/leases/renew")

	bodyStruct := struct {
		LeaseID   string `json:"lease_id"`
		Increment int    `json:"increment"`
	}{
		LeaseID:   awsCredsLease.LeaseID,
		Increment: ttl + 360, // Padded expiry to make sure we never run over
	}

	body, err := json.Marshal(bodyStruct)
	if err != nil {
		return nil, fmt.Errorf("Unable to JSON encode lease renewal body: %w", err)
	}

	r.Body = bytes.NewBuffer(body)
	resp, err := v.client.RawRequest(r)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Failed to renew AWS creds lease, got response code %d", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to read Vault response body: %w", err)
	}

	var creds VaultAWSCredsResponse
	err = json.Unmarshal(data, &creds)
	if err != nil {
		return nil, fmt.Errorf("Unable to unmarshal Vault response body: %w", err)
	}

	// Remove the padded expiry time when we return it
	expiryTime := time.Now().UTC().Add(time.Duration(creds.LeaseDuration-360) * time.Second)

	return &VaultAWSCredsLease{
		LeaseExpiryTime: expiryTime,
		LeaseID:         creds.LeaseID,
	}, nil
}
