import { config } from "../config";
import { ACCESS_TOKEN, CARDS, jsonResponse } from "../test-utils";
import { ApiError, apiRequest, describeApiError, getCards } from "./client";

function stubFetch(response: Response | Error | DOMException) {
  const fetchMock = vi.fn(() => (response instanceof Response ? Promise.resolve(response) : Promise.reject(response)));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

async function errorOf(promise: Promise<unknown>): Promise<ApiError> {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  );
  expect(error).toBeInstanceOf(ApiError);
  return error as ApiError;
}

afterEach(() => vi.unstubAllGlobals());

describe("getCards", () => {
  it("calls GET /cards with the access token as a bearer token", async () => {
    const fetchMock = stubFetch(jsonResponse({ cards: CARDS }));

    await expect(getCards(ACCESS_TOKEN)).resolves.toEqual(CARDS);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe(`${config.apiUrl}/cards`);
    expect(init.method).toBe("GET");
    expect(init.headers).toEqual({ Authorization: `Bearer ${ACCESS_TOKEN}` });
  });

  it("rejects a response with an unexpected shape", async () => {
    stubFetch(jsonResponse({ cards: [{ cardId: "card-001" }] }));
    const error = await errorOf(getCards(ACCESS_TOKEN));
    expect(error.kind).toBe("unknown");
  });
});

describe("error mapping", () => {
  it.each([
    [401, { message: "Unauthorized" }, "unauthorized", false],
    [403, { message: "Forbidden" }, "forbidden", false],
    [429, { message: "Too Many Requests" }, "throttled", true],
    [400, { error: { code: "INVALID_REQUEST", message: "cardId is required" } }, "invalid", false],
    [404, { error: { code: "CARD_NOT_FOUND", message: "no such card" } }, "not_found", false],
    [500, { error: { code: "INTERNAL_ERROR", message: "boom" } }, "server", true],
    [503, { error: { code: "QUEUE_UNAVAILABLE", message: "queue down" } }, "server", true],
  ] as const)("maps HTTP %i to %s (retryable: %s)", async (status, body, kind, retryable) => {
    stubFetch(jsonResponse(body, status));
    const error = await errorOf(apiRequest("/cards", ACCESS_TOKEN));
    expect(error.kind).toBe(kind);
    expect(error.status).toBe(status);
    expect(error.retryable).toBe(retryable);
  });

  it("keeps the Lambda error code and message", async () => {
    stubFetch(jsonResponse({ error: { code: "CARD_NOT_FOUND", message: "no such card" } }, 404));
    const error = await errorOf(apiRequest("/reservations", ACCESS_TOKEN));
    expect(error.code).toBe("CARD_NOT_FOUND");
    expect(error.message).toBe("no such card");
  });

  it("maps gateway errors by status, not by body", async () => {
    stubFetch(new Response("<html>bad gateway</html>", { status: 502 }));
    const error = await errorOf(apiRequest("/cards", ACCESS_TOKEN));
    expect(error.kind).toBe("server");
    expect(error.code).toBeUndefined();
  });

  it("maps a failed fetch to a retryable network error", async () => {
    stubFetch(new TypeError("Failed to fetch"));
    const error = await errorOf(apiRequest("/cards", ACCESS_TOKEN));
    expect(error.kind).toBe("network");
    expect(error.retryable).toBe(true);
  });

  it("lets an abort through unchanged", async () => {
    stubFetch(new DOMException("aborted", "AbortError"));
    await expect(apiRequest("/cards", ACCESS_TOKEN)).rejects.toMatchObject({ name: "AbortError" });
  });

  it("tells the trainer to sign in again on 401 and flags a wrong token type on 403", () => {
    expect(describeApiError(new ApiError("unauthorized", "x", 401))).toMatch(/sign in again/i);
    expect(describeApiError(new ApiError("forbidden", "x", 403))).toMatch(/wrong token type/i);
  });
});
