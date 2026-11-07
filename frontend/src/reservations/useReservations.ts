import { useCallback, useRef, useState } from "react";
import { ApiError, describeApiError } from "../api/client";
import type { Api } from "../api/useApi";
import { config } from "../config";
import { addAccepted, loadReservations, type StoredReservation } from "./store";
import { submitWithRetry } from "./submit";

/** What the Reserve button for one card is doing right now. */
export type CardAction =
  | { kind: "idle" }
  | { kind: "submitting"; attempt: number; maxAttempts: number }
  | { kind: "error"; message: string; /** True if clicking again resends the same requestId. */ canRetry: boolean };

const IDLE: CardAction = { kind: "idle" };

export function useReservations(api: Api | null, sub: string) {
  const [reservations, setReservations] = useState<StoredReservation[]>(() => loadReservations(sub));
  const [actions, setActions] = useState<Record<string, CardAction>>({});

  // Refs, not state: a double click must be caught before React re-renders.
  const pending = useRef(new Set<string>());
  // cardId -> requestId of a submission that failed with a retryable error. The next click for the
  // card is the same submission, so it reuses this id instead of generating a new one.
  const unfinished = useRef(new Map<string, string>());
  const reservationsRef = useRef(reservations);

  const setAction = (cardId: string, action: CardAction) =>
    setActions((previous) => ({ ...previous, [cardId]: action }));

  const isQueued = useCallback(
    (cardId: string) => reservationsRef.current.some((r) => r.cardId === cardId),
    [],
  );

  const reserve = useCallback(
    async (cardId: string) => {
      if (!api || pending.current.has(cardId) || isQueued(cardId)) return;
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
        const next = addAccepted(sub, {
          requestId: queued.requestId,
          cardId,
          submittedAt: new Date().toISOString(),
        });
        reservationsRef.current = next;
        setReservations(next);
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
    [api, sub, isQueued],
  );

  return {
    reservations,
    actionFor: (cardId: string): CardAction => actions[cardId] ?? IDLE,
    reservationFor: (cardId: string) => reservations.find((r) => r.cardId === cardId),
    reserve,
  };
}
