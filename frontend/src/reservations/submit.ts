import { ApiError } from "../api/client";

const defaultSleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/**
 * Send one submission, retrying transient failures (network, 429, 500, 503) with the SAME
 * requestId so a retry can never become a second reservation. Gives up after
 * `retryDelaysMs.length + 1` attempts and rethrows the last error. Non-retryable errors
 * (400, 401, 403, 404) are thrown immediately.
 */
export async function submitWithRetry<T>(
  send: () => Promise<T>,
  options: {
    retryDelaysMs: readonly number[];
    /** Called before each retry with the number of the attempt about to start (2, 3, ...). */
    onRetry?: (attempt: number) => void;
    sleep?: (ms: number) => Promise<void>;
  },
): Promise<T> {
  const sleep = options.sleep ?? defaultSleep;
  for (let attempt = 1; ; attempt++) {
    try {
      return await send();
    } catch (error) {
      const delay = options.retryDelaysMs[attempt - 1];
      if (!(error instanceof ApiError) || !error.retryable || delay === undefined) throw error;
      options.onRetry?.(attempt + 1);
      await sleep(delay);
    }
  }
}
