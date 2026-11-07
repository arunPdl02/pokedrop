# PokéDrop API Contract

HTTP API (API Gateway) backed by Go Lambdas. Region `us-west-2`. This is the source of truth for
request/response shapes; change it before changing the implementation.

## Conventions

- Base URL: the `ApiUrl` stack output (see [Stack outputs](#stack-outputs)).
- Requests and responses are JSON (`Content-Type: application/json`).
- Timestamps are RFC 3339 UTC strings.
- CORS: allowed origin `http://localhost:5173` (CloudFront URL added in Phase 7).

## Auth rules

- Every endpoint requires `Authorization: Bearer <Cognito access token>`, validated by the API
  Gateway JWT authorizer (issuer = the user pool, audience = the app clients). The token must carry
  the scope **`aws.cognito.signin.user.admin`**. The frontend requests it at sign-in; tokens from the
  dev client (`make token`) carry it automatically. **ID tokens are rejected with `403`** (they have no
  `scope` claim), so send the access token.
- The trainer identity is the validated token's `sub` claim. It is never read from the request
  body, path or query string.
- A trainer can read only their own reservations. Another trainer's `requestId` is
  indistinguishable from an unknown one (404).
- Missing, expired or invalid tokens are rejected by the authorizer with `401` before any Lambda
  runs. A valid token without the required scope (for example an ID token) is rejected the same way
  with `403`. As defence in depth the Lambda also returns
  `401 UNAUTHORIZED` if a request reaches it without a `sub` claim; that cannot happen while every
  route has the authorizer.

## Error shape

Errors returned by the Lambdas:

```json
{ "error": { "code": "INVALID_REQUEST", "message": "cardId is required" } }
```

`code` is stable and machine-readable; `message` is for humans and may change.
Errors produced by API Gateway itself (`401`, `403`, `429`) use the gateway's default body
(`{"message": "..."}`), so clients should branch on the HTTP status, not the body, for those.

## Status values

| Status | Returned by | Meaning |
|---|---|---|
| `QUEUED` | `POST /reservations` | The request was accepted onto the queue. **Not** a reservation. |
| `CONFIRMED` | `GET /reservations/{requestId}` | The worker recorded a successful reservation (terminal). |
| `SOLD_OUT` | `GET /reservations/{requestId}` | The worker found insufficient stock (terminal, a normal outcome). |

`Delayed` is a UI-only state (polling window exceeded). The API never returns it, and a client
timeout must never be presented as `SOLD_OUT`.

---

## GET /cards

Returns the catalog and current availability.

**Response 200**

```json
{
  "cards": [
    { "cardId": "card-001", "name": "Ember Drake", "rarity": "rare", "available": 12 }
  ]
}
```

| Field | Type | Notes |
|---|---|---|
| `cardId` | string | Stable identifier, used in reservations |
| `name` | string | Display name |
| `rarity` | string | Display only |
| `available` | integer | Current stock, always ≥ 0. May be stale by the time a reservation is processed. |

**Errors:** `401`.

---

## POST /reservations

Validates a reservation request and queues it. One card per reservation.

**Request**

```json
{ "requestId": "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b", "cardId": "card-001" }
```

| Field | Type | Rules |
|---|---|---|
| `requestId` | string | Client-generated UUID (any version, hyphenated). Case-insensitive: the API **normalizes it to lowercase** before queuing and in every response, so `ABC…` and `abc…` are the same request. The frontend generates one per submission and **reuses it on retry**. |
| `cardId` | string | Non-empty, at most 64 characters, and must exist in the catalog |

The body must be a single JSON object of at most 4096 bytes. Unknown fields are ignored; in
particular the trainer identity is never taken from the body.

**Response 202 Accepted**

```json
{ "requestId": "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b", "status": "QUEUED" }
```

- `202` means **queued, not reserved**. It is returned only after SQS has accepted the message.
- Stock is not checked or decremented here. The outcome is available from
  `GET /reservations/{requestId}`.
- Resubmitting the same `requestId` is safe: the API may return `202` again, but a repeated
  request ID never consumes inventory twice (enforced by the worker).

**Errors**

| Status | Code | When |
|---|---|---|
| 400 | `INVALID_REQUEST` | Malformed JSON, body over 4096 bytes, missing field, or `requestId` is not a UUID |
| 401 | — | Missing or invalid token |
| 404 | `CARD_NOT_FOUND` | `cardId` is not in the catalog |
| 503 | `QUEUE_UNAVAILABLE` | SQS did not accept the message. Nothing was queued; retry with the **same** `requestId`. |

---

## GET /reservations/{requestId}

Returns the outcome of the caller's reservation.

**Response 200**

```json
{
  "requestId": "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b",
  "cardId": "card-001",
  "status": "CONFIRMED",
  "updatedAt": "2026-11-05T20:15:30Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `requestId` | string | Echo of the path parameter |
| `cardId` | string | Card that was requested |
| `status` | string | `CONFIRMED` or `SOLD_OUT` |
| `updatedAt` | string | When the worker recorded the outcome |

**Not yet processed:** only the worker writes outcome records, so a request that is queued but not
yet processed returns `404 RESERVATION_NOT_FOUND`. Clients that hold the `requestId` from a
`202` should treat this 404 as "still queued" and keep polling (with bounded, increasing intervals).

**Errors**

| Status | Code | When |
|---|---|---|
| 401 | — | Missing or invalid token |
| 404 | `RESERVATION_NOT_FOUND` | Unknown `requestId`, not yet processed, **or belongs to another trainer** (ownership is concealed) |

---

## Error codes

| HTTP | Code | Source | Retry? |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | Lambda | No, fix the request |
| 401 | — | Authorizer | No, sign in again |
| 403 | — | Authorizer | No, send the access token with the required scope |
| 401 | `UNAUTHORIZED` | Lambda | No (defence in depth, see [Auth rules](#auth-rules)) |
| 404 | `CARD_NOT_FOUND` | Lambda | No |
| 404 | `RESERVATION_NOT_FOUND` | Lambda | While polling a known `requestId` |
| 429 | — | API Gateway throttling | Yes, with backoff |
| 500 | `INTERNAL_ERROR` | Lambda | Yes, with the same `requestId` |
| 503 | `QUEUE_UNAVAILABLE` | Lambda | Yes, with the same `requestId` |

## Stack outputs

Needed by the frontend and `make token`. These are values of the deployed **`pokedrop-dev`** stack
(region `us-west-2`), recorded at the end of Phase 2. They are identifiers, not secrets.

| Output | Value |
|---|---|
| `ApiUrl` | `https://t03xlkdsne.execute-api.us-west-2.amazonaws.com` |
| `UserPoolId` | `us-west-2_QmYcPxWp8` |
| `UserPoolClientId` (PKCE, frontend) | `424drtu0e1eeul6lq8kn6lo82u` |
| `DevClientId` (dev-only, `ADMIN_USER_PASSWORD_AUTH`; `make token` only, not for the frontend) | `3u4upnq7tgi2r1v1bvhfug78vk` |
| `HostedUiDomain` | `pokedrop-dev-0affe45b88e7.auth.us-west-2.amazoncognito.com` |

These change if the stack is destroyed and recreated (the hosted UI domain suffix comes from the
stack ID). Re-read them with:

```sh
aws cloudformation describe-stacks --stack-name pokedrop-dev --profile pokedrop --region us-west-2 \
  --query "Stacks[0].Outputs" --output table
```

The stack also outputs `InventoryTableName`, `ReservationsTableName` and `ReservationQueueUrl`;
those are for the backend and tooling, not the frontend.

### Frontend sign-in settings

For the PKCE client (`UserPoolClientId`), against `https://<HostedUiDomain>`:

| Setting | Value |
|---|---|
| Authorize endpoint | `/oauth2/authorize` |
| Token endpoint | `/oauth2/token` |
| Logout endpoint | `/logout` |
| `response_type` | `code`, with PKCE `code_challenge_method=S256` (implicit flow is disabled) |
| `scope` | `openid email aws.cognito.signin.user.admin` (the last one is required by the API) |
| `redirect_uri` / `logout_uri` | exactly `http://localhost:5173` (no path) |
| Token to send to the API | the **access token** (an ID token gets `403`) |

> The browser authorization-code + PKCE exchange has not been run end to end yet: Phase 2 verified
> the hosted sign-in page and the redirect rules only. Phase 4 is the first real use of it.
