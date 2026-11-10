// Package stackinfo reads facts about a deployed pokedrop stack for the tooling that runs against
// it: the integration tests and the load tools. It never changes anything.
package stackinfo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

// Region is the only region the project uses.
const Region = "us-west-2"

// Stack output names (see infra/lib/pokedrop-stack.ts).
const (
	OutputAPIURL            = "ApiUrl"
	OutputInventoryTable    = "InventoryTableName"
	OutputReservationsTable = "ReservationsTableName"
	OutputQueueURL          = "ReservationQueueUrl"
	OutputDLQURL            = "ReservationDlqUrl"
	OutputQueueARN          = "ReservationQueueArn"
	OutputWorkerFunction    = "WorkerFunctionName"
	OutputWorkerLogGroup    = "WorkerLogGroupName"
)

// StackName is the CloudFormation stack for a stage.
func StackName(stage string) string { return "pokedrop-" + stage }

// Outputs are a stack's CloudFormation outputs by name.
type Outputs map[string]string

// Require returns the value of key, or an error naming the missing output.
func (o Outputs) Require(key string) (string, error) {
	if v := o[key]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("stack output %s is missing (is the stack deployed and up to date? try `make deploy`)", key)
}

type cloudformationAPI interface {
	DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
}

// LoadOutputs reads the outputs of the stage's stack.
func LoadOutputs(ctx context.Context, cf cloudformationAPI, stage string) (Outputs, error) {
	name := StackName(stage)
	out, err := cf.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		return nil, fmt.Errorf("describe stack %s (is it deployed? try `make deploy STAGE=%s`): %w", name, stage, err)
	}
	if len(out.Stacks) == 0 {
		return nil, errors.New("stack " + name + " not found")
	}
	outputs := Outputs{}
	for _, o := range out.Stacks[0].Outputs {
		outputs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	return outputs, nil
}

// ExpiredTokenHint returns guidance when err is an expired AWS session, and "" otherwise.
func ExpiredTokenHint(err error) string {
	if err != nil && strings.Contains(err.Error(), "ExpiredToken") {
		return "AWS credentials have expired: run `aws login --profile pokedrop` and retry"
	}
	return ""
}

// SafeConcurrency is the most concurrent API requests a load test may keep in flight without the
// API Lambda and the worker competing for the account's Lambda concurrency. A throttled worker
// invocation puts its messages back on the queue with a higher receive count, so an oversized test
// could push valid messages to the dead letter queue and fake a failure.
//
// The arithmetic limit is accountLimit - workerMax - 1 (the worker at its cap, one slot spare, the
// rest for the API). That was not enough: with a limit of 10 and a worker capped at 5, a run at 4
// in flight still got 6 API throttles (HTTP 503) and 1 worker throttle (docs/benchmark.md). The
// cause is not known, so the rule now takes half of that room. This is an empirical margin, not a
// derived one; raise it only after a clean run at the higher value.
func SafeConcurrency(accountLimit, workerMax int) (int, error) {
	if workerMax <= 0 {
		return 0, errors.New("the worker's event source has no maximum concurrency, so it could use the whole account limit")
	}
	room := accountLimit - workerMax - 1
	if room < 1 {
		return 0, fmt.Errorf("account Lambda concurrency limit %d leaves no room for the API next to a worker that can use %d", accountLimit, workerMax)
	}
	return max(room/2, 1), nil
}

// Capacity is what limits how hard the stack can be driven.
type Capacity struct {
	// AccountLimit is the account's unreserved Lambda concurrency.
	AccountLimit int
	// WorkerMaxConcurrency is the worker event source's maximum concurrency.
	WorkerMaxConcurrency int
	// WorkerBatchSize is the most records the worker receives per invocation.
	WorkerBatchSize int
	// SafeConcurrency is the most concurrent API requests that fit beside the worker.
	SafeConcurrency int
}

type lambdaAPI interface {
	GetAccountSettings(context.Context, *lambda.GetAccountSettingsInput, ...func(*lambda.Options)) (*lambda.GetAccountSettingsOutput, error)
	ListEventSourceMappings(context.Context, *lambda.ListEventSourceMappingsInput, ...func(*lambda.Options)) (*lambda.ListEventSourceMappingsOutput, error)
}

// ReadCapacity reads the account's Lambda concurrency limit and the worker's event source cap.
func ReadCapacity(ctx context.Context, l lambdaAPI, workerFunction, queueARN string) (Capacity, error) {
	settings, err := l.GetAccountSettings(ctx, &lambda.GetAccountSettingsInput{})
	if err != nil {
		return Capacity{}, fmt.Errorf("get Lambda account settings: %w", err)
	}
	if settings.AccountLimit == nil {
		return Capacity{}, errors.New("Lambda account settings have no account limit")
	}
	limit := int(aws.ToInt32(settings.AccountLimit.UnreservedConcurrentExecutions))

	mappings, err := l.ListEventSourceMappings(ctx, &lambda.ListEventSourceMappingsInput{
		FunctionName:   aws.String(workerFunction),
		EventSourceArn: aws.String(queueARN),
	})
	if err != nil {
		return Capacity{}, fmt.Errorf("list event source mappings for %s: %w", workerFunction, err)
	}
	if len(mappings.EventSourceMappings) != 1 {
		return Capacity{}, fmt.Errorf("want exactly one event source mapping from the reservation queue to %s, found %d", workerFunction, len(mappings.EventSourceMappings))
	}
	workerMax := 0
	if sc := mappings.EventSourceMappings[0].ScalingConfig; sc != nil {
		workerMax = int(aws.ToInt32(sc.MaximumConcurrency))
	}
	c := Capacity{
		AccountLimit:         limit,
		WorkerMaxConcurrency: workerMax,
		WorkerBatchSize:      int(aws.ToInt32(mappings.EventSourceMappings[0].BatchSize)),
	}

	safe, err := SafeConcurrency(limit, workerMax)
	if err != nil {
		return c, err
	}
	c.SafeConcurrency = safe
	return c, nil
}
