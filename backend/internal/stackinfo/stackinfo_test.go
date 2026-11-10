package stackinfo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

type fakeCF struct {
	out *cloudformation.DescribeStacksOutput
	err error
	in  *cloudformation.DescribeStacksInput
}

func (f *fakeCF) DescribeStacks(_ context.Context, in *cloudformation.DescribeStacksInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	f.in = in
	return f.out, f.err
}

func TestLoadOutputs(t *testing.T) {
	f := &fakeCF{out: &cloudformation.DescribeStacksOutput{Stacks: []cftypes.Stack{{Outputs: []cftypes.Output{
		{OutputKey: aws.String("ApiUrl"), OutputValue: aws.String("https://api")},
		{OutputKey: aws.String("ReservationQueueUrl"), OutputValue: aws.String("https://queue")},
	}}}}}
	got, err := LoadOutputs(context.Background(), f, "test")
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(f.in.StackName) != "pokedrop-test" {
		t.Errorf("stack = %q, want pokedrop-test", aws.ToString(f.in.StackName))
	}
	if v, err := got.Require(OutputAPIURL); err != nil || v != "https://api" {
		t.Errorf("ApiUrl = %q, %v", v, err)
	}
	if _, err := got.Require(OutputDLQURL); err == nil || !strings.Contains(err.Error(), OutputDLQURL) {
		t.Errorf("a missing output must be named in the error, got %v", err)
	}
}

func TestLoadOutputsErrors(t *testing.T) {
	cause := errors.New("ValidationError: Stack does not exist")
	if _, err := LoadOutputs(context.Background(), &fakeCF{err: cause}, "test"); !errors.Is(err, cause) || !strings.Contains(err.Error(), "make deploy STAGE=test") {
		t.Errorf("err = %v, want the cause and a deploy hint", err)
	}
	if _, err := LoadOutputs(context.Background(), &fakeCF{out: &cloudformation.DescribeStacksOutput{}}, "test"); err == nil {
		t.Error("no stacks must be an error")
	}
}

func TestExpiredTokenHint(t *testing.T) {
	if ExpiredTokenHint(errors.New("operation error: ExpiredToken: token expired")) == "" {
		t.Error("expected guidance for an expired token")
	}
	if ExpiredTokenHint(errors.New("boom")) != "" || ExpiredTokenHint(nil) != "" {
		t.Error("no guidance for other errors")
	}
}

func TestSafeConcurrency(t *testing.T) {
	cases := []struct {
		limit, workerMax int
		want             int
		wantErr          bool
	}{
		{10, 5, 2, false},     // this account: half of 10 - 5 - 1 (4 still throttled)
		{1000, 5, 497, false}, // a normal account: half of 994
		{7, 5, 1, false},      // room for one: never rounds down to zero
		{8, 5, 1, false},
		{6, 5, 0, true},  // nothing left for the API
		{5, 5, 0, true},  // the worker could take it all
		{10, 0, 0, true}, // worker uncapped
	}
	for _, tc := range cases {
		got, err := SafeConcurrency(tc.limit, tc.workerMax)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("SafeConcurrency(%d, %d) = %d, %v; want %d, error=%v", tc.limit, tc.workerMax, got, err, tc.want, tc.wantErr)
		}
	}
}

type fakeLambda struct {
	settings *lambda.GetAccountSettingsOutput
	mappings *lambda.ListEventSourceMappingsOutput
	err      error
	listIn   *lambda.ListEventSourceMappingsInput
}

func (f *fakeLambda) GetAccountSettings(context.Context, *lambda.GetAccountSettingsInput, ...func(*lambda.Options)) (*lambda.GetAccountSettingsOutput, error) {
	return f.settings, f.err
}

func (f *fakeLambda) ListEventSourceMappings(_ context.Context, in *lambda.ListEventSourceMappingsInput, _ ...func(*lambda.Options)) (*lambda.ListEventSourceMappingsOutput, error) {
	f.listIn = in
	return f.mappings, nil
}

func accountWith(unreserved int32) *lambda.GetAccountSettingsOutput {
	return &lambda.GetAccountSettingsOutput{AccountLimit: &lambdatypes.AccountLimit{
		ConcurrentExecutions: unreserved, UnreservedConcurrentExecutions: aws.Int32(unreserved),
	}}
}

func mappingWithCap(max *int32) *lambda.ListEventSourceMappingsOutput {
	m := lambdatypes.EventSourceMappingConfiguration{BatchSize: aws.Int32(10)}
	if max != nil {
		m.ScalingConfig = &lambdatypes.ScalingConfig{MaximumConcurrency: max}
	}
	return &lambda.ListEventSourceMappingsOutput{EventSourceMappings: []lambdatypes.EventSourceMappingConfiguration{m}}
}

func TestReadCapacity(t *testing.T) {
	f := &fakeLambda{settings: accountWith(10), mappings: mappingWithCap(aws.Int32(5))}
	got, err := ReadCapacity(context.Background(), f, "worker-fn", "arn:queue")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Capacity{AccountLimit: 10, WorkerMaxConcurrency: 5, WorkerBatchSize: 10, SafeConcurrency: 2}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if aws.ToString(f.listIn.FunctionName) != "worker-fn" || aws.ToString(f.listIn.EventSourceArn) != "arn:queue" {
		t.Errorf("the mapping must be looked up by function and queue: %+v", f.listIn)
	}
}

func TestReadCapacityRefusesAnUncappedWorkerAndStillReportsTheNumbers(t *testing.T) {
	f := &fakeLambda{settings: accountWith(10), mappings: mappingWithCap(nil)}
	got, err := ReadCapacity(context.Background(), f, "w", "q")
	if err == nil {
		t.Fatal("an uncapped worker must be an error")
	}
	if got.AccountLimit != 10 {
		t.Errorf("the account limit must still be reported, got %+v", got)
	}
}

func TestReadCapacityNeedsExactlyOneMapping(t *testing.T) {
	two := &lambda.ListEventSourceMappingsOutput{EventSourceMappings: make([]lambdatypes.EventSourceMappingConfiguration, 2)}
	for name, m := range map[string]*lambda.ListEventSourceMappingsOutput{"none": {}, "two": two} {
		if _, err := ReadCapacity(context.Background(), &fakeLambda{settings: accountWith(10), mappings: m}, "w", "q"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := ReadCapacity(context.Background(), &fakeLambda{settings: &lambda.GetAccountSettingsOutput{}}, "w", "q"); err == nil {
		t.Error("missing account limit must be an error")
	}
	cause := errors.New("denied")
	if _, err := ReadCapacity(context.Background(), &fakeLambda{err: cause}, "w", "q"); !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the cause", err)
	}
}
