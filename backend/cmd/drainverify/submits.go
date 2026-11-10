package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// submit is one request the k6 script sent (one line of its console output).
type submit struct {
	RequestID     string  `json:"requestId"`
	CardID        string  `json:"cardId"`
	SubmittedAtMs int64   `json:"submittedAtMs"`
	RespondedAtMs int64   `json:"respondedAtMs"`
	Status        int     `json:"status"` // 0: no response (see Error)
	DurationMs    float64 `json:"durationMs"`
	Error         string  `json:"error"`
}

func (s submit) accepted() bool { return s.Status == 202 }

// k6 --console-output wraps each console.log in a logfmt line: time=... level=info msg="<quoted>".
var k6Msg = regexp.MustCompile(`msg=("(?:[^"\\]|\\.)*")`)

// unwrap returns the JSON payload of a line, which is either bare JSON or k6's logfmt wrapper.
func unwrap(line string) (string, error) {
	if strings.HasPrefix(line, "{") {
		return line, nil
	}
	m := k6Msg.FindStringSubmatch(line)
	if m == nil {
		return "", errors.New("not a JSON line or a k6 console line")
	}
	payload, err := strconv.Unquote(m[1])
	if err != nil {
		return "", fmt.Errorf("unquote k6 message: %w", err)
	}
	return payload, nil
}

// parseSubmits reads the k6 console output. Every line must be a request: a stray line means the
// file is not what the verifier thinks it is, and guessing would hide a broken run.
func parseSubmits(r io.Reader) ([]submit, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []submit
	seen := map[string]bool{}
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		payload, err := unwrap(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		var s submit
		if err := json.Unmarshal([]byte(payload), &s); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		switch {
		case s.RequestID == "":
			return nil, fmt.Errorf("line %d: no requestId", n)
		case s.SubmittedAtMs <= 0 || s.RespondedAtMs < s.SubmittedAtMs:
			return nil, fmt.Errorf("line %d: bad timestamps (submitted %d, responded %d)", n, s.SubmittedAtMs, s.RespondedAtMs)
		case seen[s.RequestID]:
			return nil, fmt.Errorf("line %d: requestId %s appears twice; every request must be unique", n, s.RequestID)
		}
		seen[s.RequestID] = true
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no requests in the file")
	}
	return out, nil
}
