package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

const validID = "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b"

func newHandler() *handler {
	return &handler{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

func event(route, body string, claims map[string]string) events.APIGatewayV2HTTPRequest {
	req := events.APIGatewayV2HTTPRequest{RouteKey: route, Body: body}
	if claims != nil {
		req.RequestContext.Authorizer = &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
			JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{Claims: claims},
		}
	}
	return req
}

func call(t *testing.T, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	t.Helper()
	resp, err := newHandler().handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned an error instead of a response: %v", err)
	}
	return resp
}

// errorCode asserts the response uses the docs/api.md error shape and returns its code.
func errorCode(t *testing.T, resp events.APIGatewayV2HTTPResponse) string {
	t.Helper()
	if resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q", resp.Headers["Content-Type"])
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("body %q is not the error shape: %v", resp.Body, err)
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error code and message are required: %q", resp.Body)
	}
	return body.Error.Code
}

var sub = map[string]string{"sub": "11111111-2222-3333-4444-555555555555"}

func TestPostReservationValidation(t *testing.T) {
	cases := map[string]string{
		"empty body":   ``,
		"malformed":    `{`,
		"no requestId": `{"cardId":"card-001"}`,
		"bad uuid":     `{"requestId":"abc","cardId":"card-001"}`,
		"no cardId":    `{"requestId":"` + validID + `"}`,
		"too large":    `{"requestId":"` + validID + `","cardId":"c","pad":"` + strings.Repeat("x", 5000) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := call(t, event(routePostReservations, body, sub))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", resp.StatusCode, resp.Body)
			}
			if code := errorCode(t, resp); code != "INVALID_REQUEST" {
				t.Fatalf("code = %s", code)
			}
		})
	}
}

// Until the queue exists a valid request must never be answered with 202 (queued).
func TestPostReservationNeverReturns202WithoutAQueue(t *testing.T) {
	resp := call(t, event(routePostReservations, `{"requestId":"`+validID+`","cardId":"card-001"}`, sub))
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("202 means queued; nothing can be queued yet")
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	errorCode(t, resp)
}

func TestPostReservationBase64Body(t *testing.T) {
	good := `{"requestId":"` + validID + `","cardId":"card-001"}`
	req := event(routePostReservations, base64.StdEncoding.EncodeToString([]byte(good)), sub)
	req.IsBase64Encoded = true
	if resp := call(t, req); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("valid base64 body should pass validation, got %d (%s)", resp.StatusCode, resp.Body)
	}

	req = event(routePostReservations, "!!! not base64 !!!", sub)
	req.IsBase64Encoded = true
	resp := call(t, req)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, resp) != "INVALID_REQUEST" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
}

func TestMissingIdentityIsUnauthorized(t *testing.T) {
	good := `{"requestId":"` + validID + `","cardId":"card-001"}`
	cases := map[string]events.APIGatewayV2HTTPRequest{
		"no authorizer":    event(routePostReservations, good, nil),
		"no claims":        event(routePostReservations, good, map[string]string{}),
		"empty sub":        event(routePostReservations, good, map[string]string{"sub": ""}),
		"other claim only": event(routePostReservations, good, map[string]string{"username": "x"}),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			resp := call(t, req)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if code := errorCode(t, resp); code != "UNAUTHORIZED" {
				t.Fatalf("code = %s", code)
			}
		})
	}
}

func TestIdentityIsNotTakenFromTheRequest(t *testing.T) {
	// A trainerId in the body, path or query must not stand in for a missing token identity.
	req := event(routePostReservations, `{"requestId":"`+validID+`","cardId":"c","trainerId":"victim"}`, nil)
	req.PathParameters = map[string]string{"trainerId": "victim"}
	req.QueryStringParameters = map[string]string{"sub": "victim"}
	req.Headers = map[string]string{"x-sub": "victim"}
	if resp := call(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestUnknownRouteIsAnInternalError(t *testing.T) {
	resp := call(t, event("DELETE /reservations", "", sub))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "INTERNAL_ERROR" {
		t.Fatalf("code = %s", code)
	}
}
