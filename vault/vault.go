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
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	VaultURLScheme  = "vault"
	VaultDefaultKey = "value"
)

// Client to replace vault paths by the secret value stored in Hashicorp Vault.
type EnvVault struct {
	client VaultAPI
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

	if os.Getenv("VAULT_TOKEN") == "" {
		err := GetToken(&vaultTokenAuthHandler{client: vaultClient})
		if err != nil {
			log.Errorf("Failure authenticating with Vault: %s", err)
		}
	}

	return EnvVault{client: vaultClient}
}

// DecryptAllEnv decrypts all env vars that contain a Vault path.  All values
// staring with `vault://` are overridden by the secret value stored in the
// path. For instance:
//    Input: ["db_url=url","db_pass=vault://secret/db_pass"]
//   Output: ["db_url=url","db_pass=ACTUAL_SECRET_PASS"]
//
//
// By default, the key used to retrieve the contents of the Secret that Vault
// returns is the string `VaultDefaultKey`. If you have more than one entry stored in a
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
