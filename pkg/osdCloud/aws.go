package osdCloud

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	awsSdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/sts"

	"github.com/openshift/osdctl/pkg/provider/aws"
	"github.com/openshift/osdctl/pkg/utils"
)

const (
	RhSreCcsAccessRolename        = "RH-SRE-CCS-Access"
	RhTechnicalSupportAccess      = "RH-Technical-Support-Access"
	OrganizationAccountAccessRole = "OrganizationAccountAccessRole"
)

// Creates a client for an assumed OrganizationAccountAccessRole
func CreateOrganizationAccountAccessClient(client aws.Client, accountId, region, sessionName, partiton string) (aws.Client, error) {

	assumeRoleCredentials, err := GenerateOrganizationAccountAccessCredentials(client, accountId, sessionName, partiton)
	if err != nil {
		return nil, err
	}

	organizationAccountAccessClient, err := aws.NewAwsClientWithInput(
		&aws.AwsClientInput{
			AccessKeyID:     *assumeRoleCredentials.AccessKeyId,
			SecretAccessKey: *assumeRoleCredentials.SecretAccessKey,
			SessionToken:    *assumeRoleCredentials.SessionToken,
			Region:          *awsSdk.String(region),
		},
	)
	if err != nil {
		return nil, err
	}

	return organizationAccountAccessClient, nil
}

// Uses the provided IAM Client to try and assume OrganizationAccountAccessRole for the given AWS Account
// This only works when the provided client is a user from the root account of an organization and the AWS account provided is a linked accounts within that organization
func GenerateOrganizationAccountAccessCredentials(client aws.Client, accountId, sessionName, partition string) (*sts.Credentials, error) {

	roleArnString := aws.GenerateRoleARN(accountId, "OrganizationAccountAccessRole")

	targetRoleArn, err := arn.Parse(roleArnString)
	if err != nil {
		return nil, err
	}

	targetRoleArn.Partition = partition

	assumeRoleOutput, err := client.AssumeRole(
		&sts.AssumeRoleInput{
			RoleArn:         awsSdk.String(targetRoleArn.String()),
			RoleSessionName: awsSdk.String(sessionName),
		},
	)
	if err != nil {
		return nil, err
	}

	return assumeRoleOutput.Credentials, nil

}

// Uses the provided IAM Client to perform the Assume Role chain needed to get to a cluster's Support Role
func GenerateSupportRoleCredentials(client aws.Client, awsAccountID, region, sessionName, targetRole string) (*sts.Credentials, error) {

	// Perform the Assume Role chain to get the jump
	jumpRoleCreds, err := GenerateJumpRoleCredentials(client, awsAccountID, region, sessionName)
	if err != nil {
		return nil, err
	}

	// Build client for jump role needed for the last step
	jumpRoleClient, err := aws.NewAwsClientWithInput(
		&aws.AwsClientInput{
			AccessKeyID:     *jumpRoleCreds.AccessKeyId,
			SecretAccessKey: *jumpRoleCreds.SecretAccessKey,
			SessionToken:    *jumpRoleCreds.SessionToken,
			Region:          *awsSdk.String(region),
		},
	)
	if err != nil {
		return nil, err
	}

	// Assume target ManagedOpenShift-Support role in the cluster's AWS Account
	targetAssumeRoleOutput, err := jumpRoleClient.AssumeRole(
		&sts.AssumeRoleInput{
			RoleArn:         awsSdk.String(targetRole),
			RoleSessionName: awsSdk.String(sessionName),
		},
	)
	if err != nil {
		return nil, err
	}

	return targetAssumeRoleOutput.Credentials, nil
}

// Preforms the Assume Role chain from IAM User to the Jump role
// This sequence stays within the Red Hat account boundary, so a failure here indicates an internal misconfiguration
func GenerateJumpRoleCredentials(client aws.Client, awsAccountID, region, sessionName string) (*sts.Credentials, error) {

	callerIdentityOutput, err := client.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, err
	}

	sreUserArn, err := arn.Parse(*callerIdentityOutput.Arn)
	if err != nil {
		return nil, err
	}

	// Assume RH-SRE-CCS-Access role
	sreCcsAccessRoleArn := aws.GenerateRoleARN(sreUserArn.AccountID, RhSreCcsAccessRolename)
	sreCcsAccessAssumeRoleOutput, err := client.AssumeRole(
		&sts.AssumeRoleInput{
			RoleArn:         awsSdk.String(sreCcsAccessRoleArn),
			RoleSessionName: awsSdk.String(sessionName),
		},
	)
	if err != nil {
		return nil, err
	}

	// Build client for RH-SRE-CCS-Access role
	sreCcsAccessRoleClient, err := aws.NewAwsClientWithInput(
		&aws.AwsClientInput{
			AccessKeyID:     *sreCcsAccessAssumeRoleOutput.Credentials.AccessKeyId,
			SecretAccessKey: *sreCcsAccessAssumeRoleOutput.Credentials.SecretAccessKey,
			SessionToken:    *sreCcsAccessAssumeRoleOutput.Credentials.SessionToken,
			Region:          *awsSdk.String(region),
		},
	)
	if err != nil {
		return nil, err
	}

	// Assume jump role
	// This will be different between stage and prod. There's probably a better way to do this that isn't hardcoding
	jumproleAccountID := os.Getenv("JUMPROLE_ACCOUNT_ID")
	jumpRoleArn := aws.GenerateRoleARN(jumproleAccountID, RhTechnicalSupportAccess)
	jumpAssumeRoleOutput, err := sreCcsAccessRoleClient.AssumeRole(
		&sts.AssumeRoleInput{
			RoleArn:         awsSdk.String(jumpRoleArn),
			RoleSessionName: awsSdk.String(sessionName),
		},
	)
	if err != nil {
		return nil, err
	}

	return jumpAssumeRoleOutput.Credentials, nil

}

// Uses the current IAM ARN to generate a role name. This should end up being RH-SRE-$kerberosID
func GenerateRoleSessionName(client aws.Client) (string, error) {

	callerIdentityOutput, err := client.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}

	roleArn, err := arn.Parse(awsSdk.StringValue(callerIdentityOutput.Arn))
	if err != nil {
		return "", err
	}

	splitArn := strings.Split(roleArn.Resource, "/")
	username := splitArn[1]

	return fmt.Sprintf("RH-SRE-%s", username), nil
}

type cloudCredentialsResponse struct {
	// ClusterID
	ClusterID string `json:"clusterID"`

	// Link to the console, optional
	ConsoleLink *string `json:"consoleLink,omitempty"`

	// Cloud credentials, optional
	Credentials *string `json:"credentials,omitempty"`

	// Region, optional
	Region *string `json:"region,omitempty"`
}

type awsCredentialsResponse struct {
	AccessKeyId     string `json:"AccessKeyId" yaml:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey" yaml:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken" yaml:"SessionToken"`
	Region          string `json:"Region" yaml:"Region"`
	Expiration      string `json:"Expiration" yaml:"Expiration"`
}

// Creates an AWS client based on a clusterid
// Requires previous log on to the correct api server via ocm login
// and tunneling to the backplane
func CreateAWSClient(clusterID string) (aws.Client, error) {
	token, err := utils.GetOCMApiServerToken()

	getUrl, err := utils.GetBackplaneURL(clusterID)
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve backplane URL for cluster %s: %s", clusterID, err)
	}

	client := http.Client{}

	request, _ := http.NewRequest("GET", getUrl, nil)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Authorization", "Bearer "+*token)
	request.Header.Set("User-Agent", "osdctl")

	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	var cloudCredentials cloudCredentialsResponse
	var awsCredentials awsCredentialsResponse
	if resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		cloudCredentials = cloudCredentialsResponse{}

		err = json.Unmarshal(bodyBytes, &cloudCredentials)
		if err != nil {
			return nil, fmt.Errorf("Unable to unmarshal cloud credentials: %s", err)
		}

		err = json.Unmarshal([]byte(*cloudCredentials.Credentials), &awsCredentials)
		if err != nil {
			return nil, fmt.Errorf("Unable to unmarshal aws credentials: %s", err)
		}
	}

	input := aws.AwsClientInput{AccessKeyID: awsCredentials.AccessKeyId, SecretAccessKey: awsCredentials.SecretAccessKey, SessionToken: awsCredentials.SessionToken, Region: *cloudCredentials.Region}

	return aws.NewAwsClientWithInput(&input)
}