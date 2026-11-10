package main

import (
	"strings"
	"testing"
)

const bareLine = `{"requestId":"3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b","cardId":"card-001","submittedAtMs":1000,"respondedAtMs":1180,"status":202,"durationMs":179.5}`

// k6 --console-output writes this shape (captured from k6 v2.3.0).
const k6Line = `time="2026-11-09T23:49:40-07:00" level=info msg="{\"requestId\":\"dba653f2-9037-4f04-af81-a9dc1b764b3b\",\"cardId\":\"card-001\",\"submittedAtMs\":1794293380393,\"respondedAtMs\":1794293380402,\"status\":0,\"durationMs\":0,\"error\":\"dial: connection refused\"}"`

func TestParseSubmitsReadsBareJSONAndK6Lines(t *testing.T) {
	got, err := parseSubmits(strings.NewReader(bareLine + "\n\n" + k6Line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	if s := got[0]; s.RequestID != "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b" || s.SubmittedAtMs != 1000 || s.RespondedAtMs != 1180 || !s.accepted() || s.DurationMs != 179.5 {
		t.Errorf("bare line parsed as %+v", got[0])
	}
	if s := got[1]; s.RequestID != "dba653f2-9037-4f04-af81-a9dc1b764b3b" || s.Status != 0 || s.accepted() || s.Error != "dial: connection refused" {
		t.Errorf("k6 line parsed as %+v", got[1])
	}
}

func TestParseSubmitsRejectsAFileThatIsNotAClearRun(t *testing.T) {
	cases := map[string]string{
		"empty file":         "",
		"only blank lines":   "\n  \n",
		"stray text":         "hello\n",
		"not json":           "{nope\n",
		"no request id":      `{"submittedAtMs":1,"respondedAtMs":2,"status":202}`,
		"no timestamps":      `{"requestId":"a","status":202}`,
		"response before it": `{"requestId":"a","submittedAtMs":10,"respondedAtMs":5,"status":202}`,
		"duplicate id":       bareLine + "\n" + bareLine,
	}
	for name, in := range cases {
		if _, err := parseSubmits(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
