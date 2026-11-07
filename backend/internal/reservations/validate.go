package reservations

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Request limits, mirrored in docs/api.md.
const (
	MaxBodyBytes = 4096
	maxCardIDLen = 64
)

// Code is a stable, machine-readable error code returned in the API error shape.
type Code string

const (
	CodeInvalidRequest      Code = "INVALID_REQUEST"
	CodeUnauthorized        Code = "UNAUTHORIZED"
	CodeCardNotFound        Code = "CARD_NOT_FOUND"
	CodeReservationNotFound Code = "RESERVATION_NOT_FOUND"
	CodeQueueUnavailable    Code = "QUEUE_UNAVAILABLE"
	CodeInternal            Code = "INTERNAL_ERROR"
)

// Error is a failure the API reports to the client with a stable code. The message is for humans
// and may change.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func invalid(msg string) *Error { return &Error{Code: CodeInvalidRequest, Message: msg} }

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeRequestID reports whether s is a hyphenated UUID and returns its lowercase form.
// Request IDs are dedupe keys, so two spellings of one UUID must never be two requests.
func NormalizeRequestID(s string) (string, bool) {
	if !uuidPattern.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// SubmitRequest is a validated POST /reservations body. RequestID is lowercase.
type SubmitRequest struct {
	RequestID string
	CardID    string
}

// submitBody is the wire shape. Pointers distinguish a missing field from an empty one. Unknown
// fields (such as a client-supplied trainerId) are ignored: identity comes from the token only.
type submitBody struct {
	RequestID *string `json:"requestId"`
	CardID    *string `json:"cardId"`
}

// ParseSubmit validates a POST /reservations body. Every failure is an *Error with
// CodeInvalidRequest.
func ParseSubmit(body []byte) (SubmitRequest, error) {
	if len(body) == 0 {
		return SubmitRequest{}, invalid("request body is required")
	}
	if len(body) > MaxBodyBytes {
		return SubmitRequest{}, invalid("request body is too large")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	var b submitBody
	if err := dec.Decode(&b); err != nil {
		return SubmitRequest{}, invalid("body must be a JSON object")
	}
	// Reject trailing content such as a second JSON value.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SubmitRequest{}, invalid("body must be a single JSON object")
	}

	if b.RequestID == nil || *b.RequestID == "" {
		return SubmitRequest{}, invalid("requestId is required")
	}
	requestID, ok := NormalizeRequestID(*b.RequestID)
	if !ok {
		return SubmitRequest{}, invalid("requestId must be a UUID")
	}

	if b.CardID == nil || *b.CardID == "" {
		return SubmitRequest{}, invalid("cardId is required")
	}
	if !validCardID(*b.CardID) {
		return SubmitRequest{}, invalid("cardId must be at most 64 printable characters")
	}

	return SubmitRequest{RequestID: requestID, CardID: *b.CardID}, nil
}

func validCardID(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxCardIDLen {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
