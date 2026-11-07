import { ApiError, type ReservationOutcome } from "../api/client";

export type PollResult = { kind: "outcome"; outcome: ReservationOutcome } | { kind: "delayed" };

function abortError(): DOMException {
  return new DOMException("Polling aborted", "AbortError");
}

export function abortableSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(abortError());
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(abortError());
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Check a request's outcome with increasing intervals until it has one or the window is used up.
 *
 *  - `check` resolving to null means "no outcome yet" (the API's 404 for a queued request): keep
 *    polling.
 *  - Transient failures (network, 429, 5xx, anything unexpected from the server) are also just
 *    "not known yet": keep polling.
 *  - 401 and 403 are rethrown: polling cannot fix them.
 *  - Running out of window resolves `delayed`. That says nothing about the reservation, and
 *    callers must not present it as a failure or as sold out.
 */
export async function pollForOutcome(options: {
  check: () => Promise<ReservationOutcome | null>;
  intervalsMs: readonly number[];
  windowMs: number;
  signal: AbortSignal;
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
  now?: () => number;
}): Promise<PollResult> {
  const { check, intervalsMs, windowMs, signal } = options;
  const sleep = options.sleep ?? abortableSleep;
  const now = options.now ?? Date.now;
  const start = now();

  for (let i = 0; ; i++) {
    const wait = intervalsMs[Math.min(i, intervalsMs.length - 1)] ?? 0;
    if (now() - start + wait > windowMs) return { kind: "delayed" };
    await sleep(wait, signal);
    try {
      const outcome = await check();
      if (outcome !== null) return { kind: "outcome", outcome };
    } catch (error) {
      if (!(error instanceof ApiError)) throw error;
      if (error.kind === "unauthorized" || error.kind === "forbidden") throw error;
      // Anything else (network, throttling, server trouble) is transient here.
    }
  }
}
