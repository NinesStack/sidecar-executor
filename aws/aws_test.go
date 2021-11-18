package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	. "github.com/smartystreets/goconvey/convey"
)

func Test_WaitForAWSCredsToBeActive(t *testing.T) {
	Convey("WaitForAWSCredsToBeActive()", t, func() {

	})
}
		//stsClient := &mockSTSClient{}
		//awsConfig := &mockAWSConfig{}
		//cfg := awsConfig.LoadDefaultConfig("key", "secret")
		//		cfg, _ := awsConfig.LoadDefaultConfig(context.Background(),
		//			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
		//				"key", "secret", "")),
		//		)
		//client := NewAWSClient(1, 10*time.Millisecond, cfg)

		//		Convey("Returns an error on timeout", func() {
		//			stsClient.shouldError = true
		//			err := client.WaitForAWSCredsToBeActive()
		//			So(err, ShouldNotBeNil)
		//			So(err.Error(), ShouldEqual, "Fatal error, timed out waiting for IAM credentials to become active")
		//		})
		//
		//		Convey("Returns success after 2 retries", func() {
		//			stsClient.shouldErrorUntilTries = 2
		//			client.AWSIAMRetryLimit = 3
		//			err := client.WaitForAWSCredsToBeActive()
		//			So(err, ShouldBeNil)
		//		})

type mockAWSConfig struct {
	shouldError bool
}

type mockSTSClient struct {
	shouldError           bool
	shouldErrorUntilTries int

	tryCount int
}

func (a *mockAWSConfig) LoadDefaultConfig(ctx context.Context, optFns ...func(*config.LoadOptions) error) (cfg aws.Config, err error) {
	if a.shouldError {
		return cfg, errors.New("intentional Error")
	}
	return cfg, nil
}

func (m *mockSTSClient) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if m.shouldErrorUntilTries > 0 {
		m.tryCount++
	}
	if m.shouldError {
		return nil, errors.New("intentional error")
	}
	if m.shouldErrorUntilTries < m.tryCount {
		return nil, errors.New("not enough retries")
	}
	return nil, nil
}
