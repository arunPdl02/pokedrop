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

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

const routePostReservations = "POST /reservations"

type handler struct {
	log *slog.Logger
}

func main() {
	h := &handler{log: slog.New(slog.NewJSONHandler(os.Stdout, nil))}
	lambda.Start(h.handle)
}

func (h *handler) handle(_ context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	// The authorizer validated the token before this Lambda ran. The trainer identity is the sub
	// claim and nothing else; it is never read from the body, path or query string.
	trainerID := trainerIDFrom(req)
	if trainerID == "" {
		return errorResponse(http.StatusUnauthorized, reservations.CodeUnauthorized, "missing trainer identity"), nil
	}

	switch req.RouteKey {
	case routePostReservations:
		return h.postReservation(req, trainerID), nil
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

func (h *handler) postReservation(req events.APIGatewayV2HTTPRequest, _ string) events.APIGatewayV2HTTPResponse {
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return errorResponse(http.StatusBadRequest, reservations.CodeInvalidRequest, "request body is not valid base64")
		}
		body = decoded
	}

	if _, err := reservations.ParseSubmit(body); err != nil {
		return fromError(err)
	}

	// TEMPORARY (removed by the next commit): there is no queue yet, so a valid request cannot be
	// accepted. It must never return 202, which means "queued".
	return errorResponse(http.StatusNotImplemented, "NOT_IMPLEMENTED", "queuing is not available yet")
}

func fromError(err error) events.APIGatewayV2HTTPResponse {
	var apiErr *reservations.Error
	if errors.As(err, &apiErr) {
		return errorResponse(statusFor(apiErr.Code), apiErr.Code, apiErr.Message)
	}
	return errorResponse(http.StatusInternalServerError, reservations.CodeInternal, "internal error")
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
