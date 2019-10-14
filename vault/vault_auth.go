package vault

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	DefaultTokenTTL  = 86400 // 1 day
	TokenGracePeriod = 3600  // 1 hour
)

// Wrapper for parts of the Hashicorp Vault API we have to do more
// work with before calling. Covers over some parts of the API that
// are hard to mock.
type TokenAuthHandler interface {
	Validate(token string) (*api.Secret, error)
	Login(username string, password string, options map[string]interface{}) (string, error)
	SetToken(token string)
}

// vaultTokenAuthHandler is an implementation of TokenAuthHandler to do the
// actual work of auth with the Vault API.
type vaultTokenAuthHandler struct {
	client *api.Client
}

// Validate is a wrapper for Vault method calls so we can mock them
// in tests.
func (f *vaultTokenAuthHandler) Validate(token string) (*api.Secret, error) {
	f.client.SetToken(token)
	return f.client.Auth().Token().LookupSelf()
}

// SetToken is just a pass through to Vault client SetToken()
func (f *vaultTokenAuthHandler) SetToken(token string) {
	f.client.SetToken(token)
}

// Login does the actual work of authenticating with a username and
// password. Makes it so we can mock out part of the call.
func (f *vaultTokenAuthHandler) Login(username string, password string,
	options map[string]interface{}) (string, error) {

	path := fmt.Sprintf("auth/userpass/login/%s", username)
	secret, err := f.client.Logical().Write(path, options)
	if err != nil {
		return "", fmt.Errorf("Error logging in: %s", err)
	}
	if secret == nil {
		return "", fmt.Errorf("Empty response from credential provider")
	}

	log.Info("Successfully authenticated with Vault")

	token, err := secret.TokenID()
	if err != nil {
		return "", err
	}

	return token, nil
}

// GetToken uses username and password auth to get a Vault Token
func GetToken(client TokenAuthHandler) (err error) {
	var token string

	if tokenFile := os.Getenv("VAULT_TOKEN_FILE"); tokenFile != "" {
		token, err = GetTokenFromFile(tokenFile)
	}

	if err == nil && token != "" {
		// Reach out to the API to make sure it's good
		t, err := client.Validate(token)

		if err == nil && t != nil {
			client.SetToken(token)
			return nil
		}

		log.Warn("Retrieved token is not valid, falling back")
	}

	if err != nil {
		log.Warn(err.Error())
	}

	// Fall back to logging in with user/pass creds
	token, err = GetTokenWithLogin(client)
	if err != nil {
		return err
	}

	client.SetToken(token)
	return nil
}

// GetTokenFromFile attempts to read a token from the Vault token file as
// specified in the environment.
func GetTokenFromFile(tokenFile string) (string, error) {
	file, err := os.Stat(tokenFile)
	if err != nil {
		return "", fmt.Errorf("Failed to stat token file %s, falling back to login", tokenFile)
	}

	ttl := time.Duration(GetTTL())
	expiryTime := time.Now().Add((0 - ttl + TokenGracePeriod) * time.Second)

	if file.ModTime().Before(expiryTime) {
		return "", errors.New("Token too close to expiry")
	}

	rawToken, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return "", err
	}

	log.Infof("Re-using Vault token from token file: %s", tokenFile)

	return strings.TrimSpace(string(rawToken)), nil
}

func GetTTL() int {
	var ttl int = DefaultTokenTTL

	if vaultTTL := os.Getenv("VAULT_TTL"); vaultTTL != "" {
		ttl, _ = strconv.Atoi(vaultTTL)
	}

	return ttl
}

// GetTokenWithLogin calls out to the Vault API and authenticates with userpass
// credentials.
func GetTokenWithLogin(client TokenAuthHandler) (string, error) {
	username := os.Getenv("VAULT_USERNAME")
	password := os.Getenv("VAULT_PASSWORD")

	if username == "" || password == "" {
		return "", fmt.Errorf("Must set VAULT_USERNAME and VAULT_PASSWORD")
	}

	ttl := GetTTL()

	options := map[string]interface{}{
		"password": password,
		"ttl":      ttl,
		"max_ttl":  ttl,
	}

	token, err := client.Login(username, password, options)
	if err != nil {
		return "", err // Errors are nicely wrapped already
	}

	if tokenFile := os.Getenv("VAULT_TOKEN_FILE"); tokenFile != "" {
		err = ioutil.WriteFile(tokenFile, []byte(token), 0600)
		if err != nil {
			log.Errorf("Error writing Vault token file: %s", err)
		}
	}

	return token, nil
}
