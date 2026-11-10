package reservations

import (
	"errors"
	"fmt"
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

func TestParseSubmitCardIDLimitCountsCharactersNotBytes(t *testing.T) {
	// 64 three-byte runes is 192 bytes but 64 characters, which the API contract allows.
	atLimit := strings.Repeat("カ", 64)
	got, err := ParseSubmit([]byte(`{"requestId":"` + validID + `","cardId":"` + atLimit + `"}`))
	if err != nil || got.CardID != atLimit {
		t.Fatalf("64 characters: got %+v, %v; want accepted", got, err)
	}
	_, err = ParseSubmit([]byte(`{"requestId":"` + validID + `","cardId":"` + atLimit + `カ"}`))
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeInvalidRequest {
		t.Fatalf("65 characters: err = %v, want INVALID_REQUEST", err)
	}
}

func TestParseSubmitRejectsMoreControlCharacters(t *testing.T) {
	for name, cardID := range map[string]string{
		"DEL":            `a\u007fb`,
		"tab":            `a\tb`,
		"newline":        `a\nb`,
		"carriage":       `a\rb`,
		"unit separator": `a\u001fb`,
	} {
		_, err := ParseSubmit([]byte(`{"requestId":"` + validID + `","cardId":"` + cardID + `"}`))
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != CodeInvalidRequest {
			t.Errorf("%s: err = %v, want INVALID_REQUEST", name, err)
		}
	}
}

func TestParseSubmitAcceptsAnyUUIDVersion(t *testing.T) {
	for name, id := range map[string]string{
		"nil UUID":  "00000000-0000-0000-0000-000000000000",
		"version 1": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"version 4": validID,
		"version 7": "018f3c5e-7b1a-7c2d-9e4f-0123456789ab",
	} {
		got, err := ParseSubmit([]byte(`{"requestId":"` + id + `","cardId":"c"}`))
		if err != nil || got.RequestID != id {
			t.Errorf("%s: got %+v, %v; want %q accepted", name, got, err, id)
		}
	}
}

func TestParseSubmitNormalizesMixedCaseRequestID(t *testing.T) {
	mixed := "3F2b8C1e-6D4a-4A57-9b0E-1c2D3e4F5a6B"
	got, err := ParseSubmit([]byte(`{"requestId":"` + mixed + `","cardId":"c"}`))
	if err != nil || got.RequestID != validID {
		t.Fatalf("got %+v, %v; want requestId %q", got, err, validID)
	}
}

// With a repeated key encoding/json keeps the last value. Pinning it shows that a client cannot
// slip a second, different requestId past validation by repeating the field: what is validated is
// what is queued.
func TestParseSubmitDuplicateKeysValidateTheValueThatIsUsed(t *testing.T) {
	got, err := ParseSubmit([]byte(`{"requestId":"not-a-uuid","requestId":"` + validID + `","cardId":"c"}`))
	if err != nil || got.RequestID != validID {
		t.Fatalf("last value valid: got %+v, %v", got, err)
	}
	if _, err := ParseSubmit([]byte(`{"requestId":"` + validID + `","requestId":"not-a-uuid","cardId":"c"}`)); err == nil {
		t.Fatal("last value invalid: expected an error even though an earlier value was valid")
	}
}

func TestErrorStringAndUnwrap(t *testing.T) {
	cause := errors.New("sqs down")
	err := &Error{Code: CodeQueueUnavailable, Message: "try again", Cause: cause}
	if got, want := err.Error(), "QUEUE_UNAVAILABLE: try again"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), cause.Error()) {
		t.Error("the cause is for logs and must not appear in the message")
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is must reach the cause")
	}
	var target *Error
	if !errors.As(fmt.Errorf("wrapped: %w", err), &target) || target.Code != CodeQueueUnavailable {
		t.Error("errors.As must find the *Error through a wrapper")
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
