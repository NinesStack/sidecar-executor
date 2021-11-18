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

type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type AWSConfig interface {
	LoadDefaultConfig(ctx context.Context, optFns ...func(*config.LoadOptions) error) (cfg aws.Config, err error)
}

type AWSClientConfig struct {
	defaultConfig aws.Config
	awsConfig     AWSConfig
}

type AWSClient struct {
	AWSIAMRetryLimit int
	AWSIAMRetrySleep time.Duration
	stsClient        STSClient
}

func NewAWSClient(retryLimit int, retrySleep time.Duration, cfg *AWSClientConfig) *AWSClient {
	client := sts.NewFromConfig(cfg.defaultConfig)
	return &AWSClient{
		AWSIAMRetryLimit: retryLimit,
		AWSIAMRetrySleep: retrySleep,
		stsClient:        client,
	}
}

// Load the IAM keys issued from Vault
func NewAWSClientConfig(accessKeyID string, secretAccessKeyID string) *AWSClientConfig {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKeyID, "")),
	)
	return &AWSClientConfig{
		defaultConfig: cfg,
	}
}

func (a *AWSClient) WaitForAWSCredsToBeActive() error {
	input := &sts.GetCallerIdentityInput{}

	var credsOk bool
	for i := 0; i < a.AWSIAMRetryLimit; i++ {
		_, err := a.stsClient.GetCallerIdentity(context.Background(), input)
		if err == nil {
			credsOk = true
			break
		}

		log.Infof("Failed %d times, %d retries left", i+1, a.AWSIAMRetryLimit-i-1)
		log.Infof("%s", err)
		time.Sleep(a.AWSIAMRetrySleep)
	}

	if credsOk {
		return nil
	} else {
		return fmt.Errorf("Fatal error, timed out waiting for IAM credentials to become active")
	}
}
