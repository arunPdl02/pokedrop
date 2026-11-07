import { useMemo } from "react";
import { useAuth } from "../auth/AuthProvider";
import {
  ApiError,
  describeApiError,
  getCards,
  postReservation,
  type Card,
  type QueuedReservation,
} from "./client";

export interface Api {
  getCards: (signal?: AbortSignal) => Promise<Card[]>;
  postReservation: (reservation: { requestId: string; cardId: string }) => Promise<QueuedReservation>;
}

/**
 * API calls bound to the signed-in trainer's access token, or null when signed out.
 * A 401 from any call ends the local session (the user must sign in again) and is still thrown
 * so the caller can stop what it was doing.
 */
export function useApi(): Api | null {
  const { state, expireSession } = useAuth();
  const accessToken = state.status === "signedIn" ? state.accessToken : null;

  return useMemo(() => {
    if (accessToken === null) return null;
    const guard = async <T,>(call: () => Promise<T>): Promise<T> => {
      try {
        return await call();
      } catch (error) {
        if (error instanceof ApiError && error.kind === "unauthorized") {
          expireSession(describeApiError(error));
        }
        throw error;
      }
    };
    return {
      getCards: (signal) => guard(() => getCards(accessToken, signal)),
      postReservation: (reservation) => guard(() => postReservation(accessToken, reservation)),
    };
  }, [accessToken, expireSession]);
}
