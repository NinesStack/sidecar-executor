package vault

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	"strings"
)

const VaultPathPrefix = "vault://"

// Client to replace vault paths by the secret valued stored in the Hashicorp Vault
type EnvVault interface {

	// DecryptAllEnv decrypts all envs that contains a Vault path.
	// All values staring with `vault://` are overridden by the secret valued stored in the path.
	// For instances:
	//  Input: ["db_url=url","db_pass=vault://secret/db_pass"]
	// Output: ["db_url=url","db_pass=ACTUAL_SECRET_PASS"]
	DecryptAllEnv(env []string) ([]string, error)

	// ReadSecretValue returns the secret value of a Vault path.
	ReadSecretValue(vaultPath string) (string, error)
}

// Our own narrowly-scoped interface for Hashicorp Vault Client
type VaultAPI interface {
	Address() string
	NewRequest(method, path string) *api.Request
	RawRequest(r *api.Request) (*api.Response, error)
}

type EnvVaultImpl struct {
	client VaultAPI
}

// NewDefaultVault returns a client using the default configuration.
//
// The default Address is https://127.0.0.1:8200, but this can be overridden by
// setting the `VAULT_ADDR` environment variable.
func NewDefaultVault() EnvVault {
	conf := api.DefaultConfig()
	log.Infof("Vault address '%s'", conf.Address)
	vaultClient, _ := api.NewClient(conf)
	return &EnvVaultImpl{client: vaultClient}
}

func (v *EnvVaultImpl) DecryptAllEnv(envs []string) ([]string, error) {
	var decryptedEnv []string
	for _, env := range envs {
		keyValue := strings.SplitN(env, "=", 2)
		key := keyValue[0]
		value := keyValue[1]
		if isVaultPath(value) {
			log.Debugf("Fetching secret value for path: '%s' [VAULT_ADDR: %s]", value, v.client.Address())
			decrypted, err := v.ReadSecretValue(value)
			if err != nil {
				return nil, err
			}
			value = decrypted
			log.Infof("Decrypted Env %s [VAULT_ADDR:%s, path: %s]", v.client.Address(), value)
		}
		decryptedEnv = append(decryptedEnv, fmt.Sprintf("%s=%s", key, value))
	}
	return decryptedEnv, nil
}

func (v *EnvVaultImpl) ReadSecretValue(vaultPath string) (string, error) {
	if !strings.HasPrefix(vaultPath, VaultPathPrefix) {
		return "", errors.Errorf("Invalid Vault path '%s', expecting prefix '%s'", vaultPath, VaultPathPrefix)
	}
	path := strings.TrimPrefix(vaultPath, VaultPathPrefix)
	secret, err := v.read(path)

	if err != nil {
		return "", errors.Errorf("Unable to fetch '%s' [VAULT_ADDR:%s] error: %s", path, v.client.Address(), err.Error())
	}

	if secret == nil {
		return "", errors.Errorf("Path '%s' not found [VAULT_ADDR:%s]", path, v.client.Address())
	}

	log.Debugf("Vault secret for path='%s', value: %s", path, secret.Data)
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
func (v *EnvVaultImpl) read(path string) (*api.Secret, error) {
	r := v.client.NewRequest("GET", "/v1/"+path)
	resp, err := v.client.RawRequest(r)
	if resp != nil {
		defer resp.Body.Close()
	}
	if resp != nil && resp.StatusCode == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return api.ParseSecret(resp.Body)
}
