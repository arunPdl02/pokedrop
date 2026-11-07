import { ApiError } from "../api/client";
import { submitWithRetry } from "./submit";

const retryable = () => new ApiError("server", "boom", 503, "QUEUE_UNAVAILABLE");

describe("submitWithRetry", () => {
  it("returns the first success without sleeping", async () => {
    const sleep = vi.fn((_ms: number) => Promise.resolve());
    const send = vi.fn(() => Promise.resolve("ok"));
    await expect(submitWithRetry(send, { retryDelaysMs: [1, 2], sleep })).resolves.toBe("ok");
    expect(send).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });

  it("retries transient errors with the configured delays, then succeeds", async () => {
    const sleep = vi.fn((_ms: number) => Promise.resolve());
    const onRetry = vi.fn();
    const send = vi
      .fn<() => Promise<string>>()
      .mockRejectedValueOnce(new ApiError("network", "offline"))
      .mockRejectedValueOnce(new ApiError("throttled", "slow down", 429))
      .mockResolvedValueOnce("ok");

    await expect(submitWithRetry(send, { retryDelaysMs: [10, 20, 30], sleep, onRetry })).resolves.toBe("ok");

    expect(send).toHaveBeenCalledTimes(3);
    expect(sleep.mock.calls.map(([ms]) => ms)).toEqual([10, 20]);
    expect(onRetry.mock.calls.map(([attempt]) => attempt)).toEqual([2, 3]);
  });

  it("gives up after retryDelaysMs.length + 1 attempts and rethrows the last error", async () => {
    const sleep = vi.fn((_ms: number) => Promise.resolve());
    const send = vi.fn(() => Promise.reject(retryable()));
    await expect(submitWithRetry(send, { retryDelaysMs: [1, 2, 3], sleep })).rejects.toMatchObject({ status: 503 });
    expect(send).toHaveBeenCalledTimes(4);
  });

  it.each([400, 401, 403, 404])("does not retry HTTP %i", async (status) => {
    const kinds = { 400: "invalid", 401: "unauthorized", 403: "forbidden", 404: "not_found" } as const;
    const send = vi.fn(() => Promise.reject(new ApiError(kinds[status as keyof typeof kinds], "no", status)));
    await expect(submitWithRetry(send, { retryDelaysMs: [1, 2], sleep: () => Promise.resolve() })).rejects.toBeInstanceOf(
      ApiError,
    );
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("does not retry errors that are not API errors", async () => {
    const send = vi.fn(() => Promise.reject(new Error("bug")));
    await expect(submitWithRetry(send, { retryDelaysMs: [1], sleep: () => Promise.resolve() })).rejects.toThrow("bug");
    expect(send).toHaveBeenCalledTimes(1);
  });
});
