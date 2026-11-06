// Command token prints a Cognito access token for a dev test user, creating the user first if
// it does not exist. It is backed by the dev-only app client (ADMIN_USER_PASSWORD_AUTH), which
// is never created for the prod stage.
//
// Credentials come from TEST_USER_EMAIL / TEST_USER_PASSWORD: the process environment wins, then
// the gitignored .env.local. The token is the only thing written to stdout (all messages go to
// stderr) so `TOKEN=$(make -s token)` works. The password is never printed.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

const (
	region               = "us-west-2"
	envEmail             = "TEST_USER_EMAIL"
	envPassword          = "TEST_USER_PASSWORD"
	outputUserPoolID     = "UserPoolId"
	outputDevClientID    = "DevClientId"
	expiredTokenGuidance = "AWS credentials have expired: run `aws login --profile pokedrop` and retry"
)

// parseDotEnv reads KEY=VALUE lines. Blank lines and #-comments are skipped, a leading `export `
// is allowed, and one pair of surrounding quotes is removed. It is deliberately not a shell:
// nothing is expanded or executed.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars[key] = value
	}
	return vars, sc.Err()
}

// credentials resolves the test user's email and password. A missing env file is fine as long
// as both values come from the environment.
func credentials(getenv func(string) string, envFile string) (email, password string, err error) {
	email, password = getenv(envEmail), getenv(envPassword)
	if email == "" || password == "" {
		if f, openErr := os.Open(envFile); openErr == nil {
			defer f.Close()
			vars, parseErr := parseDotEnv(f)
			if parseErr != nil {
				return "", "", fmt.Errorf("read %s: %w", envFile, parseErr)
			}
			if email == "" {
				email = vars[envEmail]
			}
			if password == "" {
				password = vars[envPassword]
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return "", "", fmt.Errorf("open %s: %w", envFile, openErr)
		}
	}
	if email == "" || password == "" {
		return "", "", fmt.Errorf("set %s and %s in the environment or in %s", envEmail, envPassword, envFile)
	}
	return email, password, nil
}

// userAPI is the slice of the Cognito client this command uses.
type userAPI interface {
	AdminGetUser(context.Context, *cognitoidentityprovider.AdminGetUserInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminGetUserOutput, error)
	AdminCreateUser(context.Context, *cognitoidentityprovider.AdminCreateUserInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminCreateUserOutput, error)
	AdminSetUserPassword(context.Context, *cognitoidentityprovider.AdminSetUserPasswordInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminSetUserPasswordOutput, error)
	AdminInitiateAuth(context.Context, *cognitoidentityprovider.AdminInitiateAuthInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminInitiateAuthOutput, error)
}

// ensureUser creates the user with a permanent password if it does not exist. An existing user
// is left untouched, including its password. It reports whether the user was created.
func ensureUser(ctx context.Context, api userAPI, poolID, email, password string) (bool, error) {
	_, err := api.AdminGetUser(ctx, &cognitoidentityprovider.AdminGetUserInput{
		UserPoolId: aws.String(poolID),
		Username:   aws.String(email),
	})
	if err == nil {
		return false, nil
	}
	var notFound *types.UserNotFoundException
	if !errors.As(err, &notFound) {
		return false, fmt.Errorf("look up test user: %w", err)
	}

	if _, err := api.AdminCreateUser(ctx, &cognitoidentityprovider.AdminCreateUserInput{
		UserPoolId:    aws.String(poolID),
		Username:      aws.String(email),
		MessageAction: types.MessageActionTypeSuppress,
		UserAttributes: []types.AttributeType{
			{Name: aws.String("email"), Value: aws.String(email)},
			{Name: aws.String("email_verified"), Value: aws.String("true")},
		},
	}); err != nil {
		return false, fmt.Errorf("create test user: %w", err)
	}
	if _, err := api.AdminSetUserPassword(ctx, &cognitoidentityprovider.AdminSetUserPasswordInput{
		UserPoolId: aws.String(poolID),
		Username:   aws.String(email),
		Password:   aws.String(password),
		Permanent:  true,
	}); err != nil {
		// Do not leave a user stuck in FORCE_CHANGE_PASSWORD with an unusable state unreported.
		return true, fmt.Errorf("set test user password (the user %s was created; check the password policy): %w", email, err)
	}
	return true, nil
}

// accessToken signs the user in with ADMIN_USER_PASSWORD_AUTH and returns the access token.
func accessToken(ctx context.Context, api userAPI, poolID, clientID, email, password string) (string, error) {
	out, err := api.AdminInitiateAuth(ctx, &cognitoidentityprovider.AdminInitiateAuthInput{
		UserPoolId:     aws.String(poolID),
		ClientId:       aws.String(clientID),
		AuthFlow:       types.AuthFlowTypeAdminUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": email, "PASSWORD": password},
	})
	if err != nil {
		var notAuthorized *types.NotAuthorizedException
		if errors.As(err, &notAuthorized) {
			return "", fmt.Errorf("sign in as %s failed: if the user already exists, %s in .env.local must match its password: %w", email, envPassword, err)
		}
		return "", fmt.Errorf("sign in: %w", err)
	}
	if out.ChallengeName != "" {
		return "", fmt.Errorf("sign in needs a %s challenge, which make token cannot answer", out.ChallengeName)
	}
	if out.AuthenticationResult == nil || aws.ToString(out.AuthenticationResult.AccessToken) == "" {
		return "", errors.New("sign in returned no access token")
	}
	return aws.ToString(out.AuthenticationResult.AccessToken), nil
}

// stackOutputs returns the outputs of a deployed stack as a map.
func stackOutputs(ctx context.Context, cf *cloudformation.Client, stackName string) (map[string]string, error) {
	out, err := cf.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return nil, fmt.Errorf("describe stack %s (is it deployed? try `make deploy`): %w", stackName, err)
	}
	if len(out.Stacks) == 0 {
		return nil, errors.New("stack " + stackName + " not found")
	}
	outputs := map[string]string{}
	for _, o := range out.Stacks[0].Outputs {
		outputs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	return outputs, nil
}

// requireOutputs returns the pool and dev client IDs, or an error naming what is missing.
func requireOutputs(outputs map[string]string, stackName string) (poolID, clientID string, err error) {
	poolID, clientID = outputs[outputUserPoolID], outputs[outputDevClientID]
	if poolID == "" {
		return "", "", fmt.Errorf("stack %s has no %s output (redeploy with `make deploy`)", stackName, outputUserPoolID)
	}
	if clientID == "" {
		return "", "", fmt.Errorf("stack %s has no %s output: the dev client does not exist for this stage", stackName, outputDevClientID)
	}
	return poolID, clientID, nil
}

func main() {
	stage := flag.String("stage", "dev", "stack stage (stack name: pokedrop-<stage>)")
	envFile := flag.String("env-file", "../.env.local", "file with "+envEmail+" and "+envPassword)
	flag.Parse()

	token, err := run(context.Background(), *stage, *envFile)
	if err != nil {
		if strings.Contains(err.Error(), "ExpiredToken") {
			fmt.Fprintln(os.Stderr, expiredTokenGuidance)
		}
		fmt.Fprintln(os.Stderr, "token:", err)
		os.Exit(1)
	}
	fmt.Println(token)
}

func run(ctx context.Context, stage, envFile string) (string, error) {
	if stage == "prod" {
		return "", errors.New("refusing to create test users or use the dev client on prod")
	}
	email, password, err := credentials(os.Getenv, envFile)
	if err != nil {
		return "", err
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return "", fmt.Errorf("load AWS config: %w", err)
	}
	stackName := "pokedrop-" + stage
	outputs, err := stackOutputs(ctx, cloudformation.NewFromConfig(cfg), stackName)
	if err != nil {
		return "", err
	}
	poolID, clientID, err := requireOutputs(outputs, stackName)
	if err != nil {
		return "", err
	}

	api := cognitoidentityprovider.NewFromConfig(cfg)
	created, err := ensureUser(ctx, api, poolID, email, password)
	if created {
		fmt.Fprintf(os.Stderr, "created test user %s\n", email)
	}
	if err != nil {
		return "", err
	}
	return accessToken(ctx, api, poolID, clientID, email, password)
}
