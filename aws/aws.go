// aws handles checking Hashicorp Vault issued AWS IAM keys are ready for
// use. IAM is eventually consistent and if we provide IAM keys to services
// before they are fully available we will get API errors. Most AWS SDK
// libraries will fallback to the next available credentials, which in most
// cases will be the EC2 instance-profile credentials that are available
// on mesos worker hosts
package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	log "github.com/sirupsen/logrus"
)

type AWSClientInterface interface {
	LoadAWSConfig(accessKeyID string, secretAccessKeyID string) (aws.Config, error)
	WaitForAWSCredsToBeActive(cfg aws.Config) error
}

type awsClient struct {
	AWSRetryLimit int
	AWSRetrySleep time.Duration
}

func newAwsClient(retryLimit int, retrySleep time.Duration) *awsClient {
	return &awsClient{retryLimit, retrySleep}
}

// Load the IAM keys issued from Vault
func (a *awsClient) LoadAWSClientConfig(accessKeyID string, secretAccessKeyID string) (*aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKeyID, "")),
	)
	if err != nil {
		return &cfg, fmt.Errorf("Fatal error, could not load AWS credential provider: %s", err)
	}
	return &cfg, nil
}

func (a *awsClient) LoadAWSStsClient(cfg aws.Config) *sts.Client {
	return sts.NewFromConfig(cfg)
}

func (a *awsClient) WaitForAWSCredsToBeActive(cfg aws.Config) error {
	svc := a.LoadAWSStsClient(cfg)
	input := &sts.GetCallerIdentityInput{}

	var credsOk bool
	for i := 0; i < a.AWSRetryLimit; i++ {
		_, err := svc.GetCallerIdentity(context.Background(), input)
		if err == nil {
			credsOk = true
			break
		}

		log.Infof("Failed %d times, %d retries left", i+1, a.AWSRetryLimit-i-1)
		time.Sleep(a.AWSRetrySleep)
	}

	if credsOk {
		return nil
	} else {
		return fmt.Errorf("Fatal error, timed out waiting for IAM credentials to become active")
	}
}
