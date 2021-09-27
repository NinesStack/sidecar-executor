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

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	RetryLimit = 10
	RetrySleep = 1 * time.Second
)

type Aws interface {
	WaitForAWSCredsToActivate(accessKeyID string, secretAccessKeyID string) error
}

func WaitForAWSCredsToActivate(accessKeyID string, secretAccessKeyID string) error {
	// Load the IAM keys issued from Vault
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKeyID, "")),
	)
	if err != nil {
		return fmt.Errorf("Fatal error, could not in load AWS credential provider")
	}

	svc := sts.NewFromConfig(cfg)
	input := &sts.GetCallerIdentityInput{}

	var credsOk bool
	for i := 0; i < RetryLimit; i++ {
		_, err := svc.GetCallerIdentity(context.Background(), input)
		if err == nil {
			credsOk = true
			break
		}

		// Should we pollute logs with retry log output?
		//log.Infof("Failed %d times, %d retries left", i+1, RetryLimit-i-1)
		time.Sleep(RetrySleep)
	}

	if credsOk {
		return nil
	} else {
		return fmt.Errorf("Fatal error, timed out waiting for credentials to become active")
	}
}
