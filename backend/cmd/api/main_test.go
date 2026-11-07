package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

const validID = "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b"

type fakeCatalog struct {
	cards     []reservations.Card
	exists    map[string]bool
	cardsErr  error
	existsErr error
}

func (f *fakeCatalog) Cards(context.Context) ([]reservations.Card, error) { return f.cards, f.cardsErr }
func (f *fakeCatalog) CardExists(_ context.Context, id string) (bool, error) {
	return f.exists[id], f.existsErr
}

type fakeQueue struct {
	sent []reservations.QueueMessage
	err  error
}

func (f *fakeQueue) Enqueue(_ context.Context, m reservations.QueueMessage) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

type fakeOutcomes struct {
	items map[[2]string]*reservations.Reservation
	err   error
}

func (f *fakeOutcomes) Get(_ context.Context, trainerID, requestID string) (*reservations.Reservation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items[[2]string{trainerID, requestID}], nil
}

type env struct {
	h        *handler
	catalog  *fakeCatalog
	queue    *fakeQueue
	outcomes *fakeOutcomes
}

func newEnv() *env {
	c := &fakeCatalog{exists: map[string]bool{"card-001": true}}
	q := &fakeQueue{}
	o := &fakeOutcomes{items: map[[2]string]*reservations.Reservation{}}
	return &env{
		h:        &handler{log: slog.New(slog.NewJSONHandler(io.Discard, nil)), svc: reservations.NewService(c, q, o)},
		catalog:  c,
		queue:    q,
		outcomes: o,
	}
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

func (e *env) call(t *testing.T, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	t.Helper()
	resp, err := e.h.handle(context.Background(), req)
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

var (
	subA = map[string]string{"sub": "sub-A"}
	subB = map[string]string{"sub": "sub-B"}
)

func goodBody() string { return `{"requestId":"` + validID + `","cardId":"card-001"}` }

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
			e := newEnv()
			resp := e.call(t, event(routePostReservations, body, subA))
			if resp.StatusCode != http.StatusBadRequest || errorCode(t, resp) != "INVALID_REQUEST" {
				t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
			}
			if len(e.queue.sent) != 0 {
				t.Fatal("an invalid request must never be queued")
			}
		})
	}
}

func TestPostReservationAcceptedOnlyAfterQueued(t *testing.T) {
	e := newEnv()
	resp := e.call(t, event(routePostReservations, goodBody(), subA))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d (%s)", resp.StatusCode, resp.Body)
	}
	if resp.Body != `{"requestId":"`+validID+`","status":"QUEUED"}` {
		t.Fatalf("body = %s", resp.Body)
	}
	want := reservations.QueueMessage{RequestID: validID, TrainerID: "sub-A", CardID: "card-001"}
	if len(e.queue.sent) != 1 || e.queue.sent[0] != want {
		t.Fatalf("sent %+v, want exactly [%+v]", e.queue.sent, want)
	}
}

// Invariant: 202 means queued. If SQS does not accept the message there is no 202.
func TestPostReservationQueueFailureIs503Not202(t *testing.T) {
	e := newEnv()
	e.queue.err = errors.New("sqs throttled")
	resp := e.call(t, event(routePostReservations, goodBody(), subA))
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("202 returned although nothing was queued")
	}
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, resp) != "QUEUE_UNAVAILABLE" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(resp.Body, "throttled") {
		t.Error("the underlying cause must not leak to clients")
	}
}

func TestPostReservationUnknownCard(t *testing.T) {
	e := newEnv()
	body := `{"requestId":"` + validID + `","cardId":"card-999"}`
	resp := e.call(t, event(routePostReservations, body, subA))
	if resp.StatusCode != http.StatusNotFound || errorCode(t, resp) != "CARD_NOT_FOUND" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	if len(e.queue.sent) != 0 {
		t.Fatal("nothing may be queued for an unknown card")
	}
}

func TestPostReservationInternalFailureHidesCause(t *testing.T) {
	e := newEnv()
	e.catalog.existsErr = errors.New("dynamodb: secret table name")
	resp := e.call(t, event(routePostReservations, goodBody(), subA))
	if resp.StatusCode != http.StatusInternalServerError || errorCode(t, resp) != "INTERNAL_ERROR" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(resp.Body, "secret") {
		t.Error("internal details leaked")
	}
}

// Invariant: identity is the token's sub, never anything the client sends.
func TestPostReservationIgnoresClientSuppliedIdentity(t *testing.T) {
	e := newEnv()
	body := `{"requestId":"` + validID + `","cardId":"card-001","trainerId":"victim","sub":"victim"}`
	req := event(routePostReservations, body, subA)
	req.PathParameters = map[string]string{"trainerId": "victim"}
	req.QueryStringParameters = map[string]string{"trainerId": "victim", "sub": "victim"}
	req.Headers = map[string]string{"x-trainer-id": "victim"}
	if resp := e.call(t, req); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	if len(e.queue.sent) != 1 || e.queue.sent[0].TrainerID != "sub-A" {
		t.Fatalf("queued %+v, want trainerId sub-A", e.queue.sent)
	}
}

func TestPostReservationNormalizesRequestID(t *testing.T) {
	e := newEnv()
	body := `{"requestId":"` + strings.ToUpper(validID) + `","cardId":"card-001"}`
	resp := e.call(t, event(routePostReservations, body, subA))
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(resp.Body, validID) {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	if e.queue.sent[0].RequestID != validID {
		t.Fatalf("queued %q, want the lowercase form", e.queue.sent[0].RequestID)
	}
}

func TestPostReservationBase64Body(t *testing.T) {
	e := newEnv()
	req := event(routePostReservations, base64.StdEncoding.EncodeToString([]byte(goodBody())), subA)
	req.IsBase64Encoded = true
	if resp := e.call(t, req); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid base64 body: got %d (%s)", resp.StatusCode, resp.Body)
	}

	req = event(routePostReservations, "!!! not base64 !!!", subA)
	req.IsBase64Encoded = true
	resp := e.call(t, req)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, resp) != "INVALID_REQUEST" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
}

func TestGetCards(t *testing.T) {
	e := newEnv()
	e.catalog.cards = []reservations.Card{
		{CardID: "card-002", Name: "Tide Wisp", Rarity: "common", Available: 5},
		{CardID: "card-001", Name: "Ember Drake", Rarity: "rare", Available: 10},
	}
	resp := e.call(t, event(routeGetCards, "", subA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
	want := `{"cards":[{"cardId":"card-001","name":"Ember Drake","rarity":"rare","available":10},` +
		`{"cardId":"card-002","name":"Tide Wisp","rarity":"common","available":5}]}`
	if resp.Body != want {
		t.Fatalf("body = %s", resp.Body)
	}
}

func TestGetCardsEmptyCatalogIsAnEmptyArray(t *testing.T) {
	resp := newEnv().call(t, event(routeGetCards, "", subA))
	if resp.StatusCode != http.StatusOK || resp.Body != `{"cards":[]}` {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
}

func TestGetCardsFailure(t *testing.T) {
	e := newEnv()
	e.catalog.cardsErr = errors.New("boom")
	resp := e.call(t, event(routeGetCards, "", subA))
	if resp.StatusCode != http.StatusInternalServerError || errorCode(t, resp) != "INTERNAL_ERROR" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
}

func getReservation(id string, claims map[string]string) events.APIGatewayV2HTTPRequest {
	req := event(routeGetReservation, "", claims)
	req.PathParameters = map[string]string{pathParamRequestID: id}
	return req
}

func TestGetReservationOwnerSeesItOthersGetNotFound(t *testing.T) {
	e := newEnv()
	e.outcomes.items[[2]string{"sub-A", validID}] = &reservations.Reservation{
		RequestID: validID, TrainerID: "sub-A", CardID: "card-001", Status: reservations.StatusConfirmed,
		UpdatedAt: time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC),
	}

	resp := e.call(t, getReservation(validID, subA))
	want := `{"requestId":"` + validID + `","cardId":"card-001","status":"CONFIRMED","updatedAt":"2026-11-07T01:02:03Z"}`
	if resp.StatusCode != http.StatusOK || resp.Body != want {
		t.Fatalf("owner: got %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(resp.Body, "sub-A") || strings.Contains(resp.Body, "trainerId") {
		t.Error("the trainer ID must never be serialized")
	}

	// Another trainer's request must look exactly like an unknown one.
	other := e.call(t, getReservation(validID, subB))
	unknown := e.call(t, getReservation("00000000-0000-4000-8000-000000000000", subA))
	if other.StatusCode != http.StatusNotFound || errorCode(t, other) != "RESERVATION_NOT_FOUND" {
		t.Fatalf("other trainer: got %d %s", other.StatusCode, other.Body)
	}
	if other.StatusCode != unknown.StatusCode || other.Body != unknown.Body {
		t.Fatalf("ownership leaked: other=%d %s, unknown=%d %s", other.StatusCode, other.Body, unknown.StatusCode, unknown.Body)
	}
}

func TestGetReservationEdgeCases(t *testing.T) {
	e := newEnv()
	e.outcomes.items[[2]string{"sub-A", validID}] = &reservations.Reservation{
		RequestID: validID, CardID: "card-001", Status: reservations.StatusSoldOut,
		UpdatedAt: time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC),
	}
	t.Run("uppercase path parameter finds the lowercase record", func(t *testing.T) {
		if resp := e.call(t, getReservation(strings.ToUpper(validID), subA)); resp.StatusCode != http.StatusOK {
			t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
		}
	})
	t.Run("malformed and missing ids are 404", func(t *testing.T) {
		for _, id := range []string{"nope", ""} {
			resp := e.call(t, getReservation(id, subA))
			if resp.StatusCode != http.StatusNotFound || errorCode(t, resp) != "RESERVATION_NOT_FOUND" {
				t.Fatalf("%q: got %d %s", id, resp.StatusCode, resp.Body)
			}
		}
	})
	t.Run("storage failure is a 500 that hides the cause", func(t *testing.T) {
		e.outcomes.err = errors.New("dynamodb exploded")
		defer func() { e.outcomes.err = nil }()
		resp := e.call(t, getReservation(validID, subA))
		if resp.StatusCode != http.StatusInternalServerError || strings.Contains(resp.Body, "exploded") {
			t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
		}
	})
}

func TestMissingIdentityIsUnauthorizedOnEveryRoute(t *testing.T) {
	claims := map[string]map[string]string{
		"no authorizer":    nil,
		"no claims":        {},
		"empty sub":        {"sub": ""},
		"other claim only": {"username": "x"},
	}
	for _, route := range []string{routeGetCards, routePostReservations, routeGetReservation} {
		for name, c := range claims {
			t.Run(route+"/"+name, func(t *testing.T) {
				e := newEnv()
				req := event(route, goodBody(), c)
				req.PathParameters = map[string]string{pathParamRequestID: validID}
				resp := e.call(t, req)
				if resp.StatusCode != http.StatusUnauthorized || errorCode(t, resp) != "UNAUTHORIZED" {
					t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
				}
				if len(e.queue.sent) != 0 {
					t.Fatal("an unauthenticated request must never be queued")
				}
			})
		}
	}
}

func TestUnknownRouteIsAnInternalError(t *testing.T) {
	resp := newEnv().call(t, event("DELETE /reservations", "", subA))
	if resp.StatusCode != http.StatusInternalServerError || errorCode(t, resp) != "INTERNAL_ERROR" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Body)
	}
}
