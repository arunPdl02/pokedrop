package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/arunPdl02/pokedrop/backend/internal/stackinfo"
	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

// This file is the thin AWS layer. It only reads: it never writes a table, queue or function.

type clients struct {
	cf     *cloudformation.Client
	lambda *lambda.Client
	sqs    *sqs.Client
	db     *dynamodb.Client
	cw     *cloudwatch.Client
	logs   *cloudwatchlogs.Client
}

// stack is a deployed stage and the names this tool needs from it.
type stack struct {
	stage       string
	apiURL      string
	tables      storage.TableNames
	queueURL    string
	dlqURL      string
	queueARN    string
	workerFn    string
	workerLogs  string
	apiFunction string
}

func (c *clients) loadStack(ctx context.Context, stage string) (stack, error) {
	out, err := stackinfo.LoadOutputs(ctx, c.cf, stage)
	if err != nil {
		return stack{}, err
	}
	var s stack
	s.stage = stage
	need := func(key string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = out.Require(key)
		return v
	}
	s.apiURL = strings.TrimRight(need(stackinfo.OutputAPIURL), "/")
	s.tables = storage.TableNames{Inventory: need(stackinfo.OutputInventoryTable), Reservations: need(stackinfo.OutputReservationsTable)}
	s.queueURL = need(stackinfo.OutputQueueURL)
	s.dlqURL = need(stackinfo.OutputDLQURL)
	s.queueARN = need(stackinfo.OutputQueueARN)
	s.workerFn = need(stackinfo.OutputWorkerFunction)
	s.workerLogs = need(stackinfo.OutputWorkerLogGroup)
	if err != nil {
		return stack{}, err
	}
	s.apiFunction, err = c.apiFunctionName(ctx, stage)
	return s, err
}

// apiFunctionName finds the API Lambda, which has no stack output of its own.
func (c *clients) apiFunctionName(ctx context.Context, stage string) (string, error) {
	p := cloudformation.NewListStackResourcesPaginator(c.cf, &cloudformation.ListStackResourcesInput{StackName: aws.String(stackinfo.StackName(stage))})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list stack resources: %w", err)
		}
		for _, r := range page.StackResourceSummaries {
			if aws.ToString(r.ResourceType) == "AWS::Lambda::Function" && strings.HasPrefix(aws.ToString(r.LogicalResourceId), "ApiFunction") {
				return aws.ToString(r.PhysicalResourceId), nil
			}
		}
	}
	return "", errors.New("the stack has no ApiFunction Lambda")
}

// queueTotal is every message in the queue: visible, in flight and delayed.
func (c *clients) queueTotal(ctx context.Context, url string) (int, error) {
	out, err := c.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("read queue depth: %w", err)
	}
	total := 0
	for k, v := range out.Attributes {
		var n int
		if _, err := fmt.Sscan(v, &n); err != nil {
			return 0, fmt.Errorf("queue attribute %s=%q: %w", k, v, err)
		}
		total += n
	}
	return total, nil
}

// scanRows reads every stored outcome with a consistent read, for every trainer.
func (c *clients) scanRows(ctx context.Context, table string) ([]row, error) {
	var rows []row
	p := dynamodb.NewScanPaginator(c.db, &dynamodb.ScanInput{TableName: aws.String(table), ConsistentRead: aws.Bool(true)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan reservations: %w", err)
		}
		for _, item := range page.Items {
			str := func(k string) string {
				if v, ok := item[k].(*ddbtypes.AttributeValueMemberS); ok {
					return v.Value
				}
				return ""
			}
			at, err := time.Parse(time.RFC3339, str(storage.ReservationUpdatedAt))
			if err != nil {
				return nil, fmt.Errorf("reservation %q has a bad updatedAt %q: %w", str(storage.ReservationKeyRequestID), str(storage.ReservationUpdatedAt), err)
			}
			rows = append(rows, row{
				TrainerID: str(storage.ReservationKeyTrainerID),
				RequestID: str(storage.ReservationKeyRequestID),
				CardID:    str(storage.ReservationCardID),
				Status:    str(storage.ReservationStatus),
				UpdatedAt: at.UTC(),
			})
		}
	}
	return rows, nil
}

func (c *clients) stock(ctx context.Context, table, cardID string) (int, error) {
	out, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(table),
		Key:            map[string]ddbtypes.AttributeValue{storage.InventoryKeyCardID: &ddbtypes.AttributeValueMemberS{Value: cardID}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return 0, fmt.Errorf("read stock of %s: %w", cardID, err)
	}
	n, ok := out.Item[storage.InventoryStock].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("card %s has no numeric stock (is it seeded?)", cardID)
	}
	var v int
	if _, err := fmt.Sscan(n.Value, &v); err != nil {
		return 0, fmt.Errorf("card %s stock %q: %w", cardID, n.Value, err)
	}
	return v, nil
}

func (c *clients) lambdaConfig(ctx context.Context, fn string) (lambdaConfig, error) {
	out, err := c.lambda.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String(fn)})
	if err != nil {
		return lambdaConfig{}, fmt.Errorf("read configuration of %s: %w", fn, err)
	}
	cfg := lambdaConfig{MemoryMB: int(aws.ToInt32(out.MemorySize)), TimeoutSec: int(aws.ToInt32(out.Timeout)), Runtime: string(out.Runtime)}
	for _, a := range out.Architectures {
		cfg.Architecture = string(a)
	}
	if out.Environment != nil {
		cfg.FailAfterCommit = out.Environment.Variables["FAIL_AFTER_COMMIT"]
	}
	return cfg, nil
}

// queueConfig reads the visibility timeout and, if the queue has a dead letter queue, its retry limit.
func (c *clients) queueConfig(ctx context.Context, url string) (visibilitySec, maxReceive int, err error) {
	out, err := c.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameVisibilityTimeout, sqstypes.QueueAttributeNameRedrivePolicy},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("read queue configuration: %w", err)
	}
	if _, err := fmt.Sscan(out.Attributes[string(sqstypes.QueueAttributeNameVisibilityTimeout)], &visibilitySec); err != nil {
		return 0, 0, fmt.Errorf("queue visibility timeout: %w", err)
	}
	if raw := out.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]; raw != "" {
		var policy struct {
			MaxReceiveCount int `json:"maxReceiveCount"`
		}
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return 0, 0, fmt.Errorf("queue redrive policy %q: %w", raw, err)
		}
		maxReceive = policy.MaxReceiveCount
	}
	return visibilitySec, maxReceive, nil
}

// metricSum adds up a Lambda metric over [start, end].
func (c *clients) metricSum(ctx context.Context, fn, metric string, start, end time.Time) (int, error) {
	out, err := c.cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/Lambda"),
		MetricName: aws.String(metric),
		Dimensions: []cwtypes.Dimension{{Name: aws.String("FunctionName"), Value: aws.String(fn)}},
		StartTime:  aws.Time(start.Truncate(time.Minute)),
		EndTime:    aws.Time(end),
		Period:     aws.Int32(60),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	})
	if err != nil {
		return 0, fmt.Errorf("read %s of %s: %w", metric, fn, err)
	}
	total := 0.0
	for _, d := range out.Datapoints {
		total += aws.ToFloat64(d.Sum)
	}
	return int(total + 0.5), nil
}

// countLogLines counts worker log events matching a filter pattern since start.
func (c *clients) countLogLines(ctx context.Context, group, pattern string, start time.Time) (int, error) {
	n := 0
	p := cloudwatchlogs.NewFilterLogEventsPaginator(c.logs, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:  aws.String(group),
		FilterPattern: aws.String(pattern),
		StartTime:     aws.Int64(start.UnixMilli()),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("filter logs %q: %w", pattern, err)
		}
		n += len(page.Events)
	}
	return n, nil
}

// skewFrom estimates how far the local clock is ahead of the server's (negative: behind). The HTTP
// Date header is truncated to a whole second, so the server's real time is taken as the middle of
// that second, and the local time as the middle of the request.
func skewFrom(sent, received, serverDate time.Time) time.Duration {
	mid := sent.Add(received.Sub(sent) / 2)
	return mid.Sub(serverDate.Add(500 * time.Millisecond))
}

// clockSkew measures the local clock against the API's Date header. time to final outcome compares
// a local timestamp with one the worker wrote, so a skewed clock would silently shift it. The
// request needs no token: the authorizer's 401 carries a Date header too.
func clockSkew(ctx context.Context, apiURL string) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/cards", nil)
	if err != nil {
		return 0, err
	}
	sent := time.Now()
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	received := time.Now()
	if err != nil {
		return 0, fmt.Errorf("measure clock skew: %w", err)
	}
	resp.Body.Close()
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return 0, fmt.Errorf("measure clock skew: no usable Date header: %w", err)
	}
	return skewFrom(sent, received, date), nil
}

// gitState returns the commit the working tree is on and whether the code that gets deployed or
// measured (backend, infra, Makefile, load tests) has uncommitted changes.
func gitState(repoRoot string) (commit string, dirty bool, err error) {
	rev, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	status, err := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "--", "backend", "infra", "Makefile", "tests").Output()
	if err != nil {
		return "", false, fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(string(rev)), len(strings.TrimSpace(string(status))) > 0, nil
}
