// Command api is the Lambda behind the HTTP API. It only routes API Gateway events, takes the
// trainer identity from the validated token, and maps errors to the docs/api.md error shape.
// The rules live in internal/reservations.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/arunPdl02/pokedrop/backend/internal/queue"
	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

const (
	routeGetCards         = "GET /cards"
	routePostReservations = "POST /reservations"
	routeGetReservation   = "GET /reservations/{requestId}"
	pathParamRequestID    = "requestId"
)

type handler struct {
	log *slog.Logger
	svc *reservations.Service
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	svc, err := newService(context.Background())
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	h := &handler{log: log, svc: svc}
	lambda.Start(h.handle)
}

func newService(ctx context.Context) (*reservations.Service, error) {
	tables, err := storage.TableNamesFromEnv()
	if err != nil {
		return nil, err
	}
	queueURL, err := queue.URLFromEnv()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	store := storage.NewStore(dynamodb.NewFromConfig(cfg), tables)
	publisher := queue.NewPublisher(sqs.NewFromConfig(cfg), queueURL)
	return reservations.NewService(store, publisher, store), nil
}

func (h *handler) handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	// The authorizer validated the token before this Lambda ran. The trainer identity is the sub
	// claim and nothing else; it is never read from the body, path or query string.
	trainerID := trainerIDFrom(req)
	if trainerID == "" {
		return errorResponse(http.StatusUnauthorized, reservations.CodeUnauthorized, "missing trainer identity"), nil
	}

	switch req.RouteKey {
	case routeGetCards:
		return h.getCards(ctx), nil
	case routePostReservations:
		return h.postReservation(ctx, req, trainerID), nil
	case routeGetReservation:
		return h.getReservation(ctx, req, trainerID), nil
	default:
		// An unrouted request never reaches the Lambda, so this means the gateway and the
		// handler disagree about the routes.
		h.log.Error("unhandled route", "routeKey", req.RouteKey)
		return errorResponse(http.StatusInternalServerError, reservations.CodeInternal, "internal error"), nil
	}
}

func trainerIDFrom(req events.APIGatewayV2HTTPRequest) string {
	auth := req.RequestContext.Authorizer
	if auth == nil || auth.JWT == nil {
		return ""
	}
	return auth.JWT.Claims["sub"]
}

func (h *handler) getCards(ctx context.Context) events.APIGatewayV2HTTPResponse {
	cards, err := h.svc.Cards(ctx)
	if err != nil {
		return h.fail(err)
	}
	return jsonResponse(http.StatusOK, struct {
		Cards []reservations.Card `json:"cards"`
	}{Cards: cards})
}

func (h *handler) postReservation(ctx context.Context, req events.APIGatewayV2HTTPRequest, trainerID string) events.APIGatewayV2HTTPResponse {
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return errorResponse(http.StatusBadRequest, reservations.CodeInvalidRequest, "request body is not valid base64")
		}
		body = decoded
	}

	submit, err := reservations.ParseSubmit(body)
	if err != nil {
		return h.fail(err)
	}
	// 202 means queued: Submit only returns success after the queue accepted the message.
	accepted, err := h.svc.Submit(ctx, trainerID, submit)
	if err != nil {
		return h.fail(err)
	}
	return jsonResponse(http.StatusAccepted, accepted)
}

func (h *handler) getReservation(ctx context.Context, req events.APIGatewayV2HTTPRequest, trainerID string) events.APIGatewayV2HTTPResponse {
	r, err := h.svc.Status(ctx, trainerID, req.PathParameters[pathParamRequestID])
	if err != nil {
		return h.fail(err)
	}
	return jsonResponse(http.StatusOK, r)
}

// fail maps an error to the docs/api.md error shape, logging the cause of server-side failures.
// Causes never reach the client.
func (h *handler) fail(err error) events.APIGatewayV2HTTPResponse {
	var apiErr *reservations.Error
	if !errors.As(err, &apiErr) {
		h.log.Error("unexpected error", "err", err)
		return errorResponse(http.StatusInternalServerError, reservations.CodeInternal, "internal error")
	}
	status := statusFor(apiErr.Code)
	if status >= http.StatusInternalServerError {
		h.log.Error("request failed", "code", apiErr.Code, "err", apiErr.Cause)
	}
	return errorResponse(status, apiErr.Code, apiErr.Message)
}

func statusFor(code reservations.Code) int {
	switch code {
	case reservations.CodeInvalidRequest:
		return http.StatusBadRequest
	case reservations.CodeUnauthorized:
		return http.StatusUnauthorized
	case reservations.CodeCardNotFound, reservations.CodeReservationNotFound:
		return http.StatusNotFound
	case reservations.CodeQueueUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    reservations.Code `json:"code"`
	Message string            `json:"message"`
}

func errorResponse(status int, code reservations.Code, message string) events.APIGatewayV2HTTPResponse {
	return jsonResponse(status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func jsonResponse(status int, v any) events.APIGatewayV2HTTPResponse {
	b, err := json.Marshal(v)
	if err != nil {
		// Only our own fixed types are marshalled, so this is a programming error.
		return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusInternalServerError, Body: `{"error":{"code":"INTERNAL_ERROR","message":"internal error"}}`}
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}
}
