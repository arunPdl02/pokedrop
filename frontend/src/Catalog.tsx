import { useCallback, useEffect, useState } from "react";
import { describeApiError, type Card } from "./api/client";
import { useApi } from "./api/useApi";
import { useAuth } from "./auth/AuthProvider";
import { useReservations, type ReservationStatus } from "./reservations/useReservations";

const STATUS_LABEL: Record<ReservationStatus, string> = {
  QUEUED: "Queued",
  CONFIRMED: "Confirmed",
  SOLD_OUT: "Sold out",
  DELAYED: "Delayed",
};

type CatalogState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; cards: Card[] };

export function Catalog() {
  const api = useApi();
  const { state: auth } = useAuth();
  const sub = auth.status === "signedIn" ? auth.sub : "";
  const [reloads, setReloads] = useState(0);
  const reload = useCallback(() => setReloads((n) => n + 1), []);
  // Stock changes when a reservation settles, so refresh the catalog then.
  const { actionFor, reservationFor, reserve, checkAgain } = useReservations(api, sub, { onOutcome: reload });
  const [state, setState] = useState<CatalogState>({ status: "loading" });

  useEffect(() => {
    if (!api) return;
    const controller = new AbortController();
    setState((previous) => (previous.status === "ready" ? previous : { status: "loading" }));
    api.getCards(controller.signal).then(
      (cards) => setState({ status: "ready", cards }),
      (error: unknown) => {
        if (controller.signal.aborted) return;
        setState({ status: "error", message: describeApiError(error) });
      },
    );
    return () => controller.abort();
  }, [api, reloads]);

  if (state.status === "loading") return <p>Loading cards…</p>;
  if (state.status === "error") {
    return (
      <div role="alert">
        <p>{state.message}</p>
        <button type="button" onClick={reload}>
          Try again
        </button>
      </div>
    );
  }
  return (
    <section aria-label="Cards">
      <ul className="cards">
        {state.cards.map((card) => {
          const action = actionFor(card.cardId);
          const reservation = reservationFor(card.cardId);
          // Queued and Delayed requests may still complete, so no second request for the card yet.
          const active = reservation?.status === "QUEUED" || reservation?.status === "DELAYED";
          const submitting = action.kind === "submitting";
          return (
            <li key={card.cardId} className="card">
              <h2>{card.name}</h2>
              <p className="rarity">{card.rarity}</p>
              <p className="available">{card.available} available</p>
              {reservation && (
                <p className="status" role="status" data-status={reservation.status}>
                  {STATUS_LABEL[reservation.status]}
                </p>
              )}
              {reservation?.status === "DELAYED" && (
                <>
                  <p>
                    {reservation.problem ?? "This is taking longer than usual."} Your request has not been lost.
                  </p>
                  <button type="button" onClick={() => checkAgain(reservation.requestId)}>
                    Check again
                  </button>
                </>
              )}
              {action.kind === "error" && <p role="alert">{action.message}</p>}
              <button
                type="button"
                disabled={submitting || active}
                aria-busy={submitting}
                onClick={() => void reserve(card.cardId)}
              >
                {submitting
                  ? action.attempt > 1
                    ? `Retrying (${action.attempt}/${action.maxAttempts})…`
                    : "Reserving…"
                  : action.kind === "error" && action.canRetry
                    ? "Try again"
                    : "Reserve"}
              </button>
            </li>
          );
        })}
      </ul>
      <button type="button" onClick={reload}>
        Refresh
      </button>
    </section>
  );
}
