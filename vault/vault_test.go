package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

func Capture(fn func()) string {
	capture := &bytes.Buffer{}
	log.SetOutput(capture)
	fn()
	log.SetOutput(ioutil.Discard)
	return capture.String()
}

func Test_NewDefaultVault(t *testing.T) {
	Convey("NewDefaultVault()", t, func() {
		Reset(func() { os.Unsetenv("EXECUTOR_AWS_ROLE") })

		Convey("returns a properly configured Vault client", func() {
			client := NewDefaultVault()

			So(client, ShouldNotBeNil)
			So(client.client, ShouldNotBeNil)
		})

		Convey("configures a service-specific token if required", func() {
			os.Setenv("EXECUTOR_AWS_ROLE", "some-role")

			var client EnvVault

			output := Capture(func() {
				client = NewDefaultVault()
			})

			So(client, ShouldNotBeNil)
			So(client.client, ShouldNotBeNil)

			So(output, ShouldContainSubstring, "Attempting to get a service-specific parent token with TTL to match requested AWS Role")
		})
	})
}

func Test_parseTokenTTL(t *testing.T) {
	Convey("parseTokenTTL()", t, func() {
		Convey("parses a token when formatted properly", func() {
			ttl, err := parseTokenTTL("1h1m1s")
			So(err, ShouldBeNil)
			So(ttl, ShouldEqual, 3661)
		})

		Convey("returns an error on non-duration values", func() {
			_, err := parseTokenTTL("3600")
			So(err, ShouldNotBeNil)
		})
	})
}

func Test_DecryptEnvs(t *testing.T) {
	envVault := EnvVault{client: &mockVaultAPI{}}

	Convey("When working with EnvVault", t, func() {
		Reset(func() { log.SetOutput(ioutil.Discard) })
		Convey("isVaultPath() returns false for a normal env value", func() {
			value := isVaultPath("value")
			So(value, ShouldBeFalse)

			value = isVaultPath("1234")
			So(value, ShouldBeFalse)

			value = isVaultPath("http://url")
			So(value, ShouldBeFalse)

		})

		Convey("isVaultPath() returns true for a vault key value", func() {
			value := isVaultPath("vault://test")
			So(value, ShouldBeTrue)

			value = isVaultPath("vault://secret/key1")
			So(value, ShouldBeTrue)
		})

		Convey("It processes all environment vars with Vault path", func() {
			envs := []string{"Key1=Value1", "Key2=Value2", "Key3=vault://secure/validKey"}
			expected := []string{"Key1=Value1", "Key2=Value2", "Key3=secret_value"}
			securedValue, err := envVault.DecryptAllEnv(envs)
			So(err, ShouldBeNil)
			So(securedValue, ShouldResemble, expected)
		})

		Convey("ReadsecretValue", func() {
			Convey("Fails for an invalid key", func() {
				value, err := envVault.ReadSecretValue("invalid path")
				So(value, ShouldBeEmpty)
				So(err.Error(), ShouldContainSubstring, "invalid path")

			})

			Convey("Fails for key not found", func() {
				value, err := envVault.ReadSecretValue("vault://secure/notFoundKey")
				So(value, ShouldBeEmpty)
				So(err.Error(), ShouldContainSubstring, "Path 'secure/notFoundKey' not found")
			})

			Convey("Returns a secured value for a valid key", func() {
				securedValue, err := envVault.ReadSecretValue("vault://secure/validKey")
				So(err, ShouldBeNil)
				So(securedValue, ShouldEqual, "secret_value")

			})

			Convey("Returns a secured value for a valid sub-key", func() {
				securedValue, err := envVault.ReadSecretValue("vault://secure/validKeyWithSub?key=subkey")
				So(err, ShouldBeNil)
				So(securedValue, ShouldEqual, "some_sub_value")
			})

			Convey("Returns an error on a missing sub-key", func() {
				securedValue, err := envVault.ReadSecretValue("vault://secure/validKeyWithSub?key=missing")
				So(err.Error(), ShouldContainSubstring, "Value for path 'secure/validKeyWithSub' not found")
				So(securedValue, ShouldEqual, "")
			})
		})
	})
}

func Test_GetAWSCredsLease(t *testing.T) {
	Convey("GetAWSCredsLease()", t, func() {
		envVault := EnvVault{client: &mockVaultAPI{}}

		Convey("when the policy is valid", func() {
			Convey("returns a valid response, containing credentials and lease details", func() {
				roleResponse, err := envVault.GetAWSCredsLease("valid-aws-role")
				So(err, ShouldBeNil)
				So(roleResponse, ShouldNotBeNil)

				So(roleResponse.Vars, ShouldContain, "AWS_SECRET_ACCESS_KEY=BnpDs61c12345Bqc59qYjWIl0yOCsLsOHoNpHKUk")
				So(roleResponse.Vars, ShouldContain, "AWS_ACCESS_KEY_ID=AKIAAAAAASNZNDWB3NRL")
				So(roleResponse.LeaseExpiryTime.Sub(time.Now().UTC()), ShouldBeGreaterThan, 86000)
				So(roleResponse.LeaseID, ShouldEqual, "aws/creds/valid-aws-role/qE4IBWGAlWqExurMaKPdNSgG")
			})
		})

		Convey("when the policy is invalid", func() {
			Convey("handles the error", func() {
				roleResponse, err := envVault.GetAWSCredsLease("invalid-aws-role")
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "got response code 400")
				So(roleResponse, ShouldBeNil)
			})
		})

		Convey("when bad JSON is returned", func() {
			Convey("handles the error", func() {
				roleResponse, err := envVault.GetAWSCredsLease("bad-aws-role-json")
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "Unable to unmarshal Vault response body")
				So(roleResponse, ShouldBeNil)
			})

		})
	})
}

func Test_RevokeAWSCredsLease(t *testing.T) {
	Convey("RevokeAWSCredsLease()", t, func() {
		envVault := EnvVault{client: &mockVaultAPI{}}

		// *NOTE* There is no test for an invalid lease because Vault's API is
		// a bit shit, and returns a 204 regardless.

		Convey("revokes a lease", func() {
			leaseID := "aws/creds/valid-aws-role/9ElbN9g177AAAAjftE1uLtSW"

			var capture bytes.Buffer
			log.SetOutput(&capture)
			log.SetLevel(log.DebugLevel)

			err := envVault.RevokeAWSCredsLease(leaseID, "valid-aws-role")

			log.SetOutput(ioutil.Discard)

			So(err, ShouldBeNil)
			So(capture.String(), ShouldContainSubstring, "Revoking AWS lease ID '"+leaseID)
			So(capture.String(), ShouldContainSubstring, "Lease revoked")
		})
	})
}

func Test_RenewAWSCredsLease(t *testing.T) {
	Convey("RenewAWSCredsLease()", t, func() {
		envVault := EnvVault{client: &mockVaultAPI{}}

		Convey("renews a lease", func() {
			lease := &VaultAWSCredsLease{
				LeaseID:         "aws/creds/valid-aws-role/9ElbN9g177AAAAjftE1uLtSW",
				LeaseExpiryTime: time.Now().UTC(),
			}

			var capture bytes.Buffer
			log.SetOutput(&capture)
			log.SetLevel(log.DebugLevel)

			newLease, err := envVault.RenewAWSCredsLease(lease, 1000)

			log.SetOutput(ioutil.Discard)

			So(err, ShouldBeNil)
			So(capture.String(), ShouldContainSubstring, "Renewing AWS lease ID '"+lease.LeaseID)
			So(capture.String(), ShouldNotContainSubstring, "Unable")
			So(capture.String(), ShouldNotContainSubstring, "Failed")
			So(newLease, ShouldNotBeNil)
			So(newLease.LeaseID, ShouldEqual, lease.LeaseID)

			// Round whole seconds -- Let's see if this is flaky around second boundary
			So(newLease.LeaseExpiryTime.Unix(), ShouldEqual, lease.LeaseExpiryTime.Add(1000*time.Second).Unix())
		})

		Convey("returns an error on failure to renew", func() {
			lease := &VaultAWSCredsLease{
				LeaseID:         "aws/creds/invalid-aws-role/9ElbN9g177AAAAjftE1uLtSW",
				LeaseExpiryTime: time.Now().UTC(),
			}

			newLease, err := envVault.RenewAWSCredsLease(lease, 1000)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Failed to renew AWS creds lease")
			So(newLease, ShouldBeNil)
		})

		Convey("returns an error on invalid response body", func() {
			lease := &VaultAWSCredsLease{
				LeaseID:         "aws/creds/broken-body/9ElbN9g177AAAAjftE1uLtSW",
				LeaseExpiryTime: time.Now().UTC(),
			}

			newLease, err := envVault.RenewAWSCredsLease(lease, 1000)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Unable to unmarshal Vault response body")
			So(newLease, ShouldBeNil)
		})
	})
}

func Test_MaybeRevokeToken(t *testing.T) {
	Convey("MaybeRevokeToken()", t, func() {
		envVault := EnvVault{client: &mockVaultAPI{}}

		Convey("revokes our token if we have a service-specific token", func() {
			var capture bytes.Buffer
			log.SetOutput(&capture)

			envVault.token = "test-token"
			err := envVault.MaybeRevokeToken()

			log.SetOutput(ioutil.Discard)

			So(err, ShouldBeNil)
			So(capture.String(), ShouldContainSubstring, "Revoking service-specific parent token in Vault: test-token")
			So(capture.String(), ShouldNotContainSubstring, "Failed")
			So(capture.String(), ShouldContainSubstring, "Lease revoked")
		})

		Convey("does not revoke token if we don't have a service-specific token", func() {
			var capture bytes.Buffer
			log.SetOutput(&capture)

			envVault.token = ""
			err := envVault.MaybeRevokeToken()

			log.SetOutput(ioutil.Discard)

			So(err, ShouldBeNil)
			So(capture.String(), ShouldNotContainSubstring, "Revoking service-specific parent token in Vault: test-token")
			So(capture.String(), ShouldNotContainSubstring, "Lease revoked")
		})
	})
}

type mockVaultAPI struct {
	VaultAPI
}

func (m mockVaultAPI) Address() string {
	return "http://test"
}

func (m mockVaultAPI) NewRequest(method, path string) *api.Request {
	url, _ := url.Parse(path)
	return &api.Request{URL: url}
}

func (m mockVaultAPI) RawRequest(r *api.Request) (*api.Response, error) {
	if strings.Contains(r.URL.Path, "secure/notFoundKey") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 404,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "secure/validKeyWithSub") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 200,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{ "data":{ "subkey": "some_sub_value" }}`))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "secure/validKey") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 200,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{ "data":{ "value": "secret_value" }}`))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "aws/creds/valid-aws-role") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 200,
				Body: ioutil.NopCloser(bytes.NewReader([]byte(`
					{
					   "request_id" : "933ebec3-213d-6ad5-e929-a20cd97ede43",
					   "data" : {
					      "secret_key" : "BnpDs61c12345Bqc59qYjWIl0yOCsLsOHoNpHKUk",
					      "access_key" : "AKIAAAAAASNZNDWB3NRL",
					      "security_token" : null
					   },
					   "lease_id" : "aws/creds/valid-aws-role/qE4IBWGAlWqExurMaKPdNSgG",
					   "lease_duration" : 86400,
					   "renewable" : true
					}`,
				))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "aws/creds/invalid-aws-role") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 400,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(``))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "aws/creds/bad-aws-role-json") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 200,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{`))),
			},
		}, nil
	}

	// Expire AWS creds
	if strings.Contains(r.URL.Path, "sys/leases/revoke") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 204,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(``))),
			},
		}, nil
	}

	// Renew AWS creds
	if strings.Contains(r.URL.Path, "sys/leases/renew") {
		data, _ := ioutil.ReadAll(r.Body)
		bodyStruct := struct {
			LeaseID   string `json:"lease_id"`
			Increment int    `json:"increment"`
		}{}
		_ = json.Unmarshal(data, &bodyStruct)

		statusCode := 200
		if strings.Contains(bodyStruct.LeaseID, "invalid-aws-role") {
			statusCode = 401
		}

		body := fmt.Sprintf(`
				    {
					   "lease_id" : "%s",
					   "lease_duration" : %d
					}
				`, bodyStruct.LeaseID, bodyStruct.Increment)

		if strings.Contains(bodyStruct.LeaseID, "broken-body") {
			body = "{"
		}

		return &api.Response{
			Response: &http.Response{
				StatusCode: statusCode,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(body))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "auth/token/revoke-self") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 204,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(``))),
			},
		}, nil
	}

	return nil, errors.New("Unexpected endpoint called on mock Vault client")
}
