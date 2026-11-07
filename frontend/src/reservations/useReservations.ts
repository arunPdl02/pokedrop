import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, describeApiError } from "../api/client";
import type { Api } from "../api/useApi";
import { config } from "../config";
import { pollForOutcome } from "./poll";
import { addAccepted, loadReservations, recordOutcome, type StoredReservation } from "./store";
import { submitWithRetry } from "./submit";

/** What the Reserve button for one card is doing right now. */
export type CardAction =
  | { kind: "idle" }
  | { kind: "submitting"; attempt: number; maxAttempts: number }
  | { kind: "error"; message: string; /** True if clicking again resends the same requestId. */ canRetry: boolean };

/**
 * What the trainer sees for a reservation.
 *  - QUEUED:    the API accepted it (202) and no outcome exists yet. Not a reservation.
 *  - CONFIRMED: the worker recorded a successful reservation (terminal).
 *  - SOLD_OUT:  the worker recorded insufficient stock (terminal).
 *  - DELAYED:   UI-only. We stopped polling; the request may still complete. Says nothing about stock.
 */
export type ReservationStatus = "QUEUED" | "CONFIRMED" | "SOLD_OUT" | "DELAYED";

export interface ReservationView extends StoredReservation {
  status: ReservationStatus;
  /** Set when polling stopped on an error that retrying will not fix. */
  problem?: string;
}

const IDLE: CardAction = { kind: "idle" };

interface Options {
  /** Called when a request reaches a terminal outcome during this session (stock has changed). */
  onOutcome?: () => void;
}

export function useReservations(api: Api | null, sub: string, { onOutcome }: Options = {}) {
  const [reservations, setReservations] = useState<StoredReservation[]>(() => loadReservations(sub));
  const [actions, setActions] = useState<Record<string, CardAction>>({});
  const [delayed, setDelayed] = useState<ReadonlySet<string>>(new Set());
  const [problems, setProblems] = useState<Record<string, string>>({});

  // Refs, not state: a double click must be caught before React re-renders.
  const pending = useRef(new Set<string>());
  // cardId -> requestId of a submission that failed with a retryable error. The next click for the
  // card is the same submission, so it reuses this id instead of generating a new one.
  const unfinished = useRef(new Map<string, string>());
  const reservationsRef = useRef(reservations);
  const polling = useRef(new Map<string, AbortController>());
  const onOutcomeRef = useRef(onOutcome);
  onOutcomeRef.current = onOutcome;

  const setAction = (cardId: string, action: CardAction) =>
    setActions((previous) => ({ ...previous, [cardId]: action }));

  const commit = (next: StoredReservation[]) => {
    reservationsRef.current = next;
    setReservations(next);
  };

  /** A card has an active reservation until the worker has recorded an outcome for it. */
  const hasActive = useCallback(
    (cardId: string) => reservationsRef.current.some((r) => r.cardId === cardId && !r.outcome),
    [],
  );

  const reserve = useCallback(
    async (cardId: string) => {
      if (!api || pending.current.has(cardId) || hasActive(cardId)) return;
      pending.current.add(cardId);

      const requestId = unfinished.current.get(cardId) ?? crypto.randomUUID();
      unfinished.current.set(cardId, requestId);
      const maxAttempts = config.submission.retryDelaysMs.length + 1;
      setAction(cardId, { kind: "submitting", attempt: 1, maxAttempts });

      try {
        const queued = await submitWithRetry(() => api.postReservation({ requestId, cardId }), {
          retryDelaysMs: config.submission.retryDelaysMs,
          onRetry: (attempt) => setAction(cardId, { kind: "submitting", attempt, maxAttempts }),
        });
        unfinished.current.delete(cardId);
        commit(addAccepted(sub, { requestId: queued.requestId, cardId, submittedAt: new Date().toISOString() }));
        setAction(cardId, IDLE);
      } catch (error) {
        const canRetry = error instanceof ApiError && error.retryable;
        // A rejected request (400/404) is dead: the next click is a new submission.
        if (!canRetry) unfinished.current.delete(cardId);
        setAction(cardId, { kind: "error", message: describeApiError(error), canRetry });
      } finally {
        pending.current.delete(cardId);
      }
    },
    [api, sub, hasActive],
  );

  const startPolling = useCallback(
    (requestId: string) => {
      if (!api) return;
      const controller = new AbortController();
      polling.current.set(requestId, controller);
      pollForOutcome({
        check: () => api.getReservation(requestId, controller.signal),
        intervalsMs: config.polling.intervalsMs,
        windowMs: config.polling.windowMs,
        signal: controller.signal,
      })
        .then(
          (result) => {
            if (controller.signal.aborted) return;
            if (result.kind === "outcome") {
              commit(recordOutcome(sub, requestId, result.outcome));
              onOutcomeRef.current?.();
            } else {
              setDelayed((previous) => new Set(previous).add(requestId));
            }
          },
          (error: unknown) => {
            if (controller.signal.aborted) return;
            // 401 has already ended the session (see useApi). Anything else that stops polling
            // (a 403) is shown on the card; the trainer can ask for another check.
            if (error instanceof ApiError && error.kind === "unauthorized") return;
            setProblems((previous) => ({ ...previous, [requestId]: describeApiError(error) }));
          },
        )
        .finally(() => {
          if (polling.current.get(requestId) === controller) polling.current.delete(requestId);
        });
    },
    [api, sub],
  );

  // Poll every accepted request that has no outcome yet, including ones restored after a refresh.
  useEffect(() => {
    for (const { requestId, outcome } of reservations) {
      if (outcome || delayed.has(requestId) || problems[requestId] || polling.current.has(requestId)) continue;
      startPolling(requestId);
    }
  }, [reservations, delayed, problems, startPolling]);

  useEffect(
    () => () => {
      for (const controller of polling.current.values()) controller.abort();
      polling.current.clear();
    },
    [],
  );

  /** Resume polling for a delayed request (a fresh window). */
  const checkAgain = useCallback((requestId: string) => {
    setDelayed((previous) => {
      const next = new Set(previous);
      next.delete(requestId);
      return next;
    });
    setProblems(({ [requestId]: _dropped, ...rest }) => rest);
  }, []);

  const viewOf = (record: StoredReservation): ReservationView => {
    const problem = problems[record.requestId];
    if (record.outcome) return { ...record, status: record.outcome.status };
    return { ...record, status: delayed.has(record.requestId) || problem ? "DELAYED" : "QUEUED", problem };
  };

  return {
    actionFor: (cardId: string): CardAction => actions[cardId] ?? IDLE,
    /** The most recent reservation for a card, if any. */
    reservationFor: (cardId: string): ReservationView | undefined => {
      const latest = reservations.findLast((r) => r.cardId === cardId);
      return latest && viewOf(latest);
    },
    reserve,
    checkAgain,
  };
}
