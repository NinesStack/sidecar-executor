package vault

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	"strings"
)

const VaultPathPrefix = "vault://"

// Client to replace vault paths by the secret valued stored in the Hashicorp Vault
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
	return EnvVault{client: vaultClient}
}

// DecryptAllEnv decrypts all envs that contains a Vault path.
// All values staring with `vault://` are overridden by the secret valued stored in the path.
// For instances:
//  Input: ["db_url=url","db_pass=vault://secret/db_pass"]
// Output: ["db_url=url","db_pass=ACTUAL_SECRET_PASS"]
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
func (v EnvVault) ReadSecretValue(vaultPath string) (string, error) {
	if !strings.HasPrefix(vaultPath, VaultPathPrefix) {
		return "", errors.Errorf("Invalid Vault path '%s', expecting prefix '%s'", vaultPath, VaultPathPrefix)
	}
	path := strings.TrimPrefix(vaultPath, VaultPathPrefix)
	secret, err := v.read(path)

	if err != nil {
		return "", errors.Errorf("Unable to fetch '%s' [VAULT_ADDR:%s] error: %s", path, v.client.Address(), err)
	}

	if secret == nil {
		return "", errors.Errorf("Path '%s' not found [VAULT_ADDR:%s]", path, v.client.Address())
	}

	value, ok := secret.Data["value"].(string)

	if !ok {
		return "", errors.Errorf("Value for path '%s' not found [VAULT_ADDR:%s]", path, v.client.Address())
	}

	return value, nil
}

func isVaultPath(value string) bool {
	hasPrefix := strings.HasPrefix(value, VaultPathPrefix)
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
