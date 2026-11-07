package reservations

import (
	"errors"
	"strings"
	"testing"
)

const validID = "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b"

func TestParseSubmitAccepts(t *testing.T) {
	cases := map[string]struct {
		body string
		want SubmitRequest
	}{
		"valid": {
			`{"requestId":"` + validID + `","cardId":"card-001"}`,
			SubmitRequest{RequestID: validID, CardID: "card-001"},
		},
		"uppercase UUID is normalized": {
			`{"requestId":"` + strings.ToUpper(validID) + `","cardId":"card-001"}`,
			SubmitRequest{RequestID: validID, CardID: "card-001"},
		},
		"surrounding whitespace in JSON": {
			"  \n{\"requestId\":\"" + validID + "\",\"cardId\":\"c\"}\n ",
			SubmitRequest{RequestID: validID, CardID: "c"},
		},
		"unknown fields are ignored": {
			`{"requestId":"` + validID + `","cardId":"card-001","trainerId":"someone-else","x":1}`,
			SubmitRequest{RequestID: validID, CardID: "card-001"},
		},
		"card id at the length limit": {
			`{"requestId":"` + validID + `","cardId":"` + strings.Repeat("a", 64) + `"}`,
			SubmitRequest{RequestID: validID, CardID: strings.Repeat("a", 64)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseSubmit([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSubmitRejects(t *testing.T) {
	cases := map[string]string{
		"empty body":               ``,
		"not json":                 `nope`,
		"truncated json":           `{"requestId":`,
		"json array":               `[]`,
		"json null":                `null`,
		"json string":              `"hello"`,
		"two objects":              `{"requestId":"` + validID + `","cardId":"c"}{"a":1}`,
		"trailing garbage":         `{"requestId":"` + validID + `","cardId":"c"} x`,
		"missing requestId":        `{"cardId":"card-001"}`,
		"empty requestId":          `{"requestId":"","cardId":"card-001"}`,
		"null requestId":           `{"requestId":null,"cardId":"card-001"}`,
		"requestId wrong type":     `{"requestId":123,"cardId":"card-001"}`,
		"requestId not a uuid":     `{"requestId":"not-a-uuid","cardId":"card-001"}`,
		"uuid without hyphens":     `{"requestId":"3f2b8c1e6d4a4a579b0e1c2d3e4f5a6b","cardId":"card-001"}`,
		"uuid with braces":         `{"requestId":"{` + validID + `}","cardId":"card-001"}`,
		"uuid with trailing space": `{"requestId":"` + validID + ` ","cardId":"card-001"}`,
		"uuid with newline":        `{"requestId":"` + validID + `\n","cardId":"card-001"}`,
		"missing cardId":           `{"requestId":"` + validID + `"}`,
		"empty cardId":             `{"requestId":"` + validID + `","cardId":""}`,
		"cardId wrong type":        `{"requestId":"` + validID + `","cardId":7}`,
		"cardId too long":          `{"requestId":"` + validID + `","cardId":"` + strings.Repeat("a", 65) + `"}`,
		"cardId control char":      `{"requestId":"` + validID + `","cardId":"a\u0000b"}`,
		"body too large":           `{"requestId":"` + validID + `","cardId":"c","pad":"` + strings.Repeat("x", MaxBodyBytes) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSubmit([]byte(body))
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("got %v, want *Error", err)
			}
			if apiErr.Code != CodeInvalidRequest {
				t.Fatalf("code = %s, want %s", apiErr.Code, CodeInvalidRequest)
			}
			if apiErr.Message == "" {
				t.Fatal("message must not be empty")
			}
		})
	}
}

func TestParseSubmitBodyAtLimit(t *testing.T) {
	prefix := `{"requestId":"` + validID + `","cardId":"c","pad":"`
	suffix := `"}`
	body := prefix + strings.Repeat("x", MaxBodyBytes-len(prefix)-len(suffix)) + suffix
	if len(body) != MaxBodyBytes {
		t.Fatalf("test body is %d bytes", len(body))
	}
	if _, err := ParseSubmit([]byte(body)); err != nil {
		t.Fatalf("a body of exactly %d bytes must be accepted: %v", MaxBodyBytes, err)
	}
}

func TestNormalizeRequestID(t *testing.T) {
	if got, ok := NormalizeRequestID("3F2B8C1E-6D4A-4A57-9B0E-1C2D3E4F5A6B"); !ok || got != validID {
		t.Fatalf("got %q %v", got, ok)
	}
	for _, bad := range []string{"", "x", validID + "0", " " + validID} {
		if _, ok := NormalizeRequestID(bad); ok {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
