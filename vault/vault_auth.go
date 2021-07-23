package vault

import (
	"encoding/json"
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
	DefaultTokenTTL    = 86400 // 1 day
	TokenGracePeriod   = 3600  // 1 hour
	StartupGracePeriod = 600   // 10 minutes
)

// Wrapper for parts of the Hashicorp Vault API we have to do more
// work with before calling. Covers over some parts of the API that
// are hard to mock.
type TokenAuthHandler interface {
	Validate(token string) (*api.Secret, error)
	Login(username string, password string, options map[string]interface{}) (string, error)
	Renew(token string, ttl int) error
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

	// Vault has a shit API
	if len(options) > 1 {
		return "",
			errors.New("You can ONLY pass a `password` option to Login(), received more")
	}

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

	log.Debugf("Auth Response: %#v", secret)

	return token, nil
}

// Renew does just that with a vault Token. Returns an error if anything went
// wrong. Otherwise, you got the TTL you asked for... as far as we can tell.
func (f *vaultTokenAuthHandler) Renew(token string, ttl int) error {
	// Pass our token, and the TTL, which will be the whole increment
	options := map[string]interface{}{
		"token":     token,
		"increment": ttl,
	}

	resp, err := f.client.Logical().Write("auth/token/renew", options)
	if err != nil {
		return fmt.Errorf("Error renewing token: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("Empty response from credential provider")
	}

	// Connect up and find out our new TTL
	resp, err = f.client.Auth().Token().Lookup(token)
	if err != nil {
		return fmt.Errorf("Unable to look up token: %s", err)
	}
	if resp == nil {
		return errors.New("Invalid response from token lookup")
	}

	ttlRaw, ok := resp.Data["ttl"].(json.Number)
	if !ok {
		return errors.New("No ttl value found in data object for token")
	}
	ttlInt, _ := ttlRaw.Int64()

	if ttlInt + 10 < int64(ttl) { // We have to account for the API call time. 10 seconds is plenty
		log.Warnf("FAILED to get TTL we asked for! Asked for: %d, Got: %d", ttl, ttlInt)
	} else {
		log.Infof("Successfully renewed token with Vault. New TTL: %d", ttlInt)
	}
	return nil
}

// GetToken uses username and password auth to get a Vault Token
func GetToken(client TokenAuthHandler) error {
	var (
		token string
		err   error
	)

	if tokenFile := os.Getenv("VAULT_TOKEN_FILE"); tokenFile != "" {
		token, err = GetTokenFromFile(tokenFile)
	}

	if err == nil && token != "" {
		// Reach out to the API to make sure it's good
		_, err := client.Validate(token)
		if err == nil {
			client.SetToken(token)
			return nil
		}
		log.Warnf("Retrieved token is not valid: %s. Re-logging in", err)
	}

	if err != nil {
		log.Warn(err.Error())
	}

	// Fall back to logging in with user/pass creds
	ttl := GetTTL()
	token, err = GetTokenWithLogin(client, ttl)
	if err != nil {
		return err
	}

	// Store the token for other executors
	CacheToken(token)
	return nil
}

// CacheToken caches the token for all the other executors to use
func CacheToken(token string) {
	if tokenFile := os.Getenv("VAULT_TOKEN_FILE"); tokenFile != "" {
		err := ioutil.WriteFile(tokenFile, []byte(token), 0600)
		if err != nil {
			log.Errorf("Error writing Vault token file: %s", err)
		}
	}
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

	token := strings.TrimSpace(string(rawToken))

	return token, nil
}

// GetTTL attempts to grab a TTL from the environment and then falls back to
// the configured default if none is found.
func GetTTL() int {
	var ttl int = DefaultTokenTTL

	if vaultTTL := os.Getenv("VAULT_TTL"); vaultTTL != "" {
		ttl, _ = strconv.Atoi(vaultTTL)
	}

	return ttl
}

// GetTokenWithLogin calls out to the Vault API and authenticates with userpass
// credentials.
func GetTokenWithLogin(client TokenAuthHandler, ttl int) (string, error) {
	username := os.Getenv("VAULT_USERNAME")
	password := os.Getenv("VAULT_PASSWORD")

	if username == "" || password == "" {
		return "", fmt.Errorf("Must set VAULT_USERNAME and VAULT_PASSWORD")
	}

	options := map[string]interface{}{"password": password}

	token, err := client.Login(username, password, options)
	if err != nil {
		return "", err // Errors are nicely wrapped already
	}

	client.SetToken(token)

	// Attempt to renew with the proper TTL
	err = client.Renew(token, ttl+StartupGracePeriod)
	if err != nil {
		return "", err // Errors are nicely wrapped already
	}

	return token, nil
}
