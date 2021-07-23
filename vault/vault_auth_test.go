package vault

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"errors"

	"github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

func Test_GetTokenFromFile(t *testing.T) {
	Convey("GetTokenFromFile()", t, func() {
		log.SetOutput(ioutil.Discard)
		tmpdir, _ := ioutil.TempDir("", "testing")
		tmpfn := filepath.Join(tmpdir, "vault-token")

		Convey("Reads a file if it exists", func() {
			err := ioutil.WriteFile(tmpfn, []byte("bogus_data"), 0600)
			So(err, ShouldBeNil)
			token, err := GetTokenFromFile(tmpfn)
			So(err, ShouldBeNil)
			So(token, ShouldEqual, "bogus_data")
		})

		Convey("Returns an error when the file is missing", func() {
			token, err := GetTokenFromFile(tmpfn)
			So(err.Error(), ShouldContainSubstring, "Failed to stat")
			So(token, ShouldEqual, "")
		})
		// TODO no clean way to test expiry ATM as it depends on file ModTime
		// too invasive to mock as-is.
	})
}

func Test_GetTTL(t *testing.T) {
	Convey("GetTTL()", t, func() {
		Convey("Returns the default value when the env var is not set", func() {
			So(GetTTL(), ShouldEqual, DefaultTokenTTL)
		})

		Convey("Returns the specified value if set", func() {
			Reset(func() { os.Unsetenv("VAULT_TTL") })

			os.Setenv("VAULT_TTL", "666")
			So(GetTTL(), ShouldEqual, 666)
		})
	})
}

func Test_GetToken(t *testing.T) {
	Convey("GetToken()", t, func() {
		mock := &mockTokenAuthHandler{}

		Convey("when VAULT_TOKEN_FILE is NOT set", func() {
			os.Unsetenv("VAULT_TOKEN_FILE")

			os.Setenv("VAULT_USERNAME", "lancelot")
			os.Setenv("VAULT_PASSWORD", "guinevere")

			Reset(func() {
				os.Unsetenv("VAULT_USERNAME")
				os.Unsetenv("VAULT_PASSWORD")
			})

			Convey("goes straight to Login", func() {
				mock.token = "from_login"

				err := GetToken(mock)
				So(err, ShouldBeNil)
				So(mock.LoginWasCalled, ShouldBeTrue)
				So(mock.ValidateWasCalled, ShouldBeFalse)
				So(mock.token, ShouldEqual, "from_login")
				So(mock.loginOptions["password"], ShouldEqual, "guinevere")
			})

			Convey("errors when Login fails", func() {
				mock.token = "from_login"
				mock.LoginShouldError = true

				err := GetToken(mock)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "intentional error")
				So(mock.LoginWasCalled, ShouldBeTrue)
				So(mock.ValidateWasCalled, ShouldBeFalse)
				So(mock.token, ShouldEqual, "from_login") // check it _wasn't_ overwritten
			})

			Convey("errors when env vars aren't set", func() {
				os.Unsetenv("VAULT_USERNAME")
				os.Unsetenv("VAULT_PASSWORD")

				mock.token = "from_login"

				err := GetToken(mock)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "set VAULT_USERNAME and VAULT_PASSWORD")
				So(mock.LoginWasCalled, ShouldBeFalse)
				So(mock.ValidateWasCalled, ShouldBeFalse)
				So(mock.token, ShouldEqual, "from_login") // check it _wasn't_ overwritten
			})
		})

		Convey("when VAULT_TOKEN_FILE is set", func() {
			tmpdir, _ := ioutil.TempDir("", "testing")
			tmpfn := filepath.Join(tmpdir, "vault-token")

			os.Setenv("VAULT_TOKEN_FILE", tmpfn)
			os.Setenv("VAULT_USERNAME", "lancelot")
			os.Setenv("VAULT_PASSWORD", "guinevere")

			Reset(func() {
				os.Unsetenv("VAULT_TOKEN_FILE")
				os.Unsetenv("VAULT_USERNAME")
				os.Unsetenv("VAULT_PASSWORD")
			})

			Convey("sets a token from the file when present", func() {
				err := ioutil.WriteFile(tmpfn, []byte("bogus_data"), 0600)
				So(err, ShouldBeNil)
				mock.ValidateShouldError = false

				err = GetToken(mock)
				So(err, ShouldBeNil)
				So(mock.LoginWasCalled, ShouldBeFalse)
				So(mock.ValidateWasCalled, ShouldBeTrue)
				So(mock.token, ShouldEqual, "bogus_data")
			})

			Convey("falls back to userpass when the file is invalid", func() {
				err := ioutil.WriteFile(tmpfn, []byte("bogus_data"), 0600)
				So(err, ShouldBeNil)
				mock.ValidateShouldError = true

				err = GetToken(mock)
				So(err, ShouldBeNil)
				So(mock.LoginWasCalled, ShouldBeTrue)
				So(mock.ValidateWasCalled, ShouldBeTrue)
				So(mock.token, ShouldEqual, "")
			})

			Convey("falls back to userpass when the file is missing", func() {
				// We don't write the file, so the error should bubble up
				err := GetToken(mock)
				So(err, ShouldBeNil)
				So(mock.LoginWasCalled, ShouldBeTrue)
				So(mock.ValidateWasCalled, ShouldBeFalse)
			})
		})
	})
}

type mockTokenAuthHandler struct {
	token string

	LoginWasCalled    bool
	ValidateWasCalled bool
	SetTokenWasCalled bool

	LoginShouldError    bool
	ValidateShouldError bool

	loginOptions map[string]interface{}
}

func (m *mockTokenAuthHandler) Validate(token string) (*api.Secret, error) {
	m.ValidateWasCalled = true
	if m.ValidateShouldError {
		return nil, errors.New("intentional error")
	}

	return &api.Secret{}, nil
}

func (m *mockTokenAuthHandler) Login(username string, password string,
	options map[string]interface{}) (string, error) {

	m.loginOptions = options
	m.LoginWasCalled = true
	if m.LoginShouldError {
		return "", errors.New("intentional error")
	}

	return m.token, nil
}

func (m *mockTokenAuthHandler) Renew(token string, ttl int) error {
	return nil
}

func (m *mockTokenAuthHandler) SetToken(token string) {
	m.token = token
	m.SetTokenWasCalled = true
}
