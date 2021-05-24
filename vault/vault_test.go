package vault

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

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

func Test_GetAWSRoleVars(t *testing.T) {
	Convey("GetAWSRoleVars()", t, func() {
		envVault := EnvVault{client: &mockVaultAPI{}}

		Convey("when the policy is valid", func() {
			Convey("returns a valid response, containing credentials and lease details", func() {
				roleResponse, err := envVault.GetAWSRoleVars("valid-aws-role")
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
				roleResponse, err := envVault.GetAWSRoleVars("invalid-aws-role")
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "got response code 400")
				So(roleResponse, ShouldBeNil)
			})
		})

		Convey("when bad JSON is returned", func() {
			Convey("handles the error", func() {
				roleResponse, err := envVault.GetAWSRoleVars("bad-aws-role-json")
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "Unable to unmarshal Vault response body")
				So(roleResponse, ShouldBeNil)
			})

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
				Body: ioutil.NopCloser(bytes.NewReader([]byte(``))),
			},
		}, nil
	}

	if strings.Contains(r.URL.Path, "aws/creds/bad-aws-role-json") {
		return &api.Response{
			Response: &http.Response{
				StatusCode: 200,
				Body: ioutil.NopCloser(bytes.NewReader([]byte(`{`))),
			},
		}, nil
	}

	return nil, errors.New("Unexpected endpoint called on mock Vault client")
}
