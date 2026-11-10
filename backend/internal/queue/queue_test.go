package queue

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

type fakeSQS struct {
	in       []*sqs.SendMessageInput
	deadline []time.Time // the context deadline seen by each send (zero if none)
	out      *sqs.SendMessageOutput
	err      error
}

func (f *fakeSQS) SendMessage(ctx context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.in = append(f.in, in)
	d, _ := ctx.Deadline()
	f.deadline = append(f.deadline, d)
	return f.out, f.err
}

var msg = reservations.QueueMessage{RequestID: "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b", TrainerID: "sub-A", CardID: "card-001"}

func TestEnqueueSendsTheMessageBody(t *testing.T) {
	f := &fakeSQS{out: &sqs.SendMessageOutput{MessageId: aws.String("m-1")}}
	if err := NewPublisher(f, "https://queue").Enqueue(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(f.in) != 1 || *f.in[0].QueueUrl != "https://queue" {
		t.Fatalf("inputs: %+v", f.in)
	}
	var got reservations.QueueMessage
	if err := json.Unmarshal([]byte(*f.in[0].MessageBody), &got); err != nil || got != msg {
		t.Fatalf("body %s -> %+v %v", *f.in[0].MessageBody, got, err)
	}
	want := `{"requestId":"3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b","trainerId":"sub-A","cardId":"card-001"}`
	if *f.in[0].MessageBody != want {
		t.Fatalf("wire format changed (the worker depends on it): %s", *f.in[0].MessageBody)
	}
}

func TestEnqueueFailsUnlessSQSAccepted(t *testing.T) {
	cases := map[string]*fakeSQS{
		"send error":    {err: errors.New("throttled")},
		"no output":     {},
		"no message id": {out: &sqs.SendMessageOutput{}},
	}
	for name, f := range cases {
		if err := NewPublisher(f, "u").Enqueue(context.Background(), msg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A stuck queue must fail the request promptly (a 503) rather than hang until the gateway gives up,
// so every send carries a deadline of at most sendTimeout, even if the caller's context has none.
func TestEnqueueBoundsTheSendWithATimeout(t *testing.T) {
	f := &fakeSQS{out: &sqs.SendMessageOutput{MessageId: aws.String("m-1")}}
	start := time.Now()
	if err := NewPublisher(f, "u").Enqueue(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if f.deadline[0].IsZero() {
		t.Fatal("the send had no deadline")
	}
	if remaining := f.deadline[0].Sub(start); remaining > sendTimeout+time.Second {
		t.Fatalf("deadline is %v away, want at most %v", remaining, sendTimeout)
	}
}

func TestEnqueueKeepsTheSendErrorForLogs(t *testing.T) {
	cause := errors.New("throttled")
	err := NewPublisher(&fakeSQS{err: cause}, "u").Enqueue(context.Background(), msg)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the SQS error", err)
	}
}

func TestURLFromEnv(t *testing.T) {
	t.Setenv(EnvQueueURL, "")
	if _, err := URLFromEnv(); err == nil {
		t.Error("expected error when unset")
	}
	t.Setenv(EnvQueueURL, "https://queue")
	if u, err := URLFromEnv(); err != nil || u != "https://queue" {
		t.Errorf("got %q %v", u, err)
	}
}
