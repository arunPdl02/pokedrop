import { config } from "../config";

export interface Card {
  cardId: string;
  name: string;
  rarity: string;
  /** Current stock, always >= 0 but possibly stale by the time a reservation is processed. */
  available: number;
}

/**
 * What a failed call means to the UI. Gateway errors (401/403/429) have no stable body, so the kind
 * is derived from the HTTP status; `code` is only set for errors returned by the Lambdas.
 *  - unauthorized: 401, the token is missing/expired/invalid -> sign in again
 *  - forbidden:    403, a token without the required scope (an ID token) -> a bug, send the access token
 *  - invalid:      400, fix the request (no retry)
 *  - not_found:    404 (CARD_NOT_FOUND, RESERVATION_NOT_FOUND)
 *  - throttled:    429, retry with backoff
 *  - server:       500/503 (INTERNAL_ERROR, QUEUE_UNAVAILABLE), retry with the same requestId
 *  - network:      the request never got a response, retry with the same requestId
 */
export type ApiErrorKind =
  | "unauthorized"
  | "forbidden"
  | "invalid"
  | "not_found"
  | "throttled"
  | "server"
  | "network"
  | "unknown";

export class ApiError extends Error {
  constructor(
    readonly kind: ApiErrorKind,
    message: string,
    readonly status?: number,
    readonly code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  get retryable(): boolean {
    return this.kind === "throttled" || this.kind === "server" || this.kind === "network";
  }
}

function kindForStatus(status: number): ApiErrorKind {
  if (status === 401) return "unauthorized";
  if (status === 403) return "forbidden";
  if (status === 400) return "invalid";
  if (status === 404) return "not_found";
  if (status === 429) return "throttled";
  if (status >= 500) return "server";
  return "unknown";
}

async function errorFrom(response: Response): Promise<ApiError> {
  let code: string | undefined;
  let message = `Request failed (${response.status})`;
  try {
    const body = (await response.json()) as { error?: { code?: unknown; message?: unknown }; message?: unknown };
    if (typeof body.error?.code === "string") code = body.error.code;
    if (typeof body.error?.message === "string") message = body.error.message;
    else if (typeof body.message === "string") message = body.message;
  } catch {
    // Not JSON; keep the generic message.
  }
  return new ApiError(kindForStatus(response.status), message, response.status, code);
}

export async function apiRequest(
  path: string,
  accessToken: string,
  init: {
    method?: "GET" | "POST";
    body?: unknown;
    signal?: AbortSignal;
    /** A 2xx whose body cannot be read is still a success (the server accepted the request). */
    tolerateUnreadableBody?: boolean;
  } = {},
): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(`${config.apiUrl}${path}`, {
      method: init.method ?? "GET",
      headers: {
        // Always the ACCESS token: the authorizer rejects ID tokens with 403.
        Authorization: `Bearer ${accessToken}`,
        ...(init.body !== undefined && { "Content-Type": "application/json" }),
      },
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      signal: init.signal,
    });
  } catch (error) {
    if ((error as { name?: unknown } | null)?.name === "AbortError") throw error;
    throw new ApiError("network", "Could not reach the server.");
  }
  if (!response.ok) throw await errorFrom(response);
  return response.json().catch(() => {
    if (init.tolerateUnreadableBody) return null;
    throw new ApiError("unknown", "The server returned an unreadable response.", response.status);
  });
}

function isCard(value: unknown): value is Card {
  if (typeof value !== "object" || value === null) return false;
  const card = value as Record<string, unknown>;
  return (
    typeof card.cardId === "string" &&
    typeof card.name === "string" &&
    typeof card.rarity === "string" &&
    typeof card.available === "number"
  );
}

export async function getCards(accessToken: string, signal?: AbortSignal): Promise<Card[]> {
  const body = (await apiRequest("/cards", accessToken, { signal })) as { cards?: unknown };
  if (!Array.isArray(body?.cards) || !body.cards.every(isCard)) {
    throw new ApiError("unknown", "The catalog response had an unexpected shape.");
  }
  return body.cards;
}

export interface QueuedReservation {
  requestId: string;
  status: "QUEUED";
}

/**
 * POST /reservations. A 202 means the request was QUEUED, not reserved. Resending the same
 * requestId is always safe; the worker never consumes inventory twice for one requestId.
 */
export async function postReservation(
  accessToken: string,
  reservation: { requestId: string; cardId: string },
  signal?: AbortSignal,
): Promise<QueuedReservation> {
  const body = (await apiRequest("/reservations", accessToken, {
    method: "POST",
    body: reservation,
    signal,
    tolerateUnreadableBody: true,
  })) as { requestId?: unknown } | null;
  // The API lowercases the id; prefer its echo so what we store matches what we poll.
  const requestId = typeof body?.requestId === "string" ? body.requestId : reservation.requestId;
  return { requestId, status: "QUEUED" };
}

export interface ReservationOutcome {
  status: "CONFIRMED" | "SOLD_OUT";
  /** When the worker recorded the outcome (RFC 3339). */
  updatedAt: string;
}

/**
 * GET /reservations/{requestId}. Resolves to null while the request has no outcome yet: only the
 * worker writes outcome records, so a queued-but-unprocessed request answers 404
 * RESERVATION_NOT_FOUND, which means "still queued", not "failed".
 */
export async function getReservation(
  accessToken: string,
  requestId: string,
  signal?: AbortSignal,
): Promise<ReservationOutcome | null> {
  let body: unknown;
  try {
    body = await apiRequest(`/reservations/${encodeURIComponent(requestId)}`, accessToken, { signal });
  } catch (error) {
    if (error instanceof ApiError && error.kind === "not_found" && error.code === "RESERVATION_NOT_FOUND") {
      return null;
    }
    throw error;
  }
  const { status, updatedAt } = (body ?? {}) as { status?: unknown; updatedAt?: unknown };
  if ((status !== "CONFIRMED" && status !== "SOLD_OUT") || typeof updatedAt !== "string") {
    throw new ApiError("unknown", "The reservation response had an unexpected shape.");
  }
  return { status, updatedAt };
}

/** A message suitable for showing to a trainer. */
export function describeApiError(error: unknown): string {
  if (!(error instanceof ApiError)) return "Something went wrong.";
  switch (error.kind) {
    case "unauthorized":
      return "Your session expired. Please sign in again.";
    case "forbidden":
      return "The server rejected this sign-in token (wrong token type). This is a bug; please report it.";
    case "network":
      return "Could not reach the server. Check your connection and try again.";
    case "throttled":
      return "Too many requests. Please wait a moment and try again.";
    case "server":
      return "The server had a problem. Please try again.";
    case "not_found":
      return error.code === "CARD_NOT_FOUND" ? "That card is no longer in the catalog." : error.message;
    default:
      return error.message;
  }
}
