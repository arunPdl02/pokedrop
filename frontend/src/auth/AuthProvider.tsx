import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  clearSession,
  completeSignIn,
  isAuthRedirect,
  loadSession,
  signOut as signOutOfCognito,
  startSignIn,
  subFromToken,
  type Session,
} from "./auth";

export type AuthState =
  | { status: "loading" }
  | { status: "signedOut"; message?: string }
  | { status: "signedIn"; accessToken: string; sub: string; expiresAt: number };

export interface AuthContextValue {
  state: AuthState;
  signIn: () => void;
  signOut: () => void;
  /** Drop the local session, e.g. after the API answered 401. */
  expireSession: (message?: string) => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

function toState(session: Session | null): AuthState {
  if (!session) return { status: "signedOut" };
  const sub = subFromToken(session.accessToken);
  if (!sub) return { status: "signedOut", message: "The access token was not usable. Please sign in again." };
  return { status: "signedIn", accessToken: session.accessToken, sub, expiresAt: session.expiresAt };
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(() =>
    isAuthRedirect() ? { status: "loading" } : toState(loadSession()),
  );

  // Finish the redirect from the hosted sign-in page.
  useEffect(() => {
    if (state.status !== "loading") return;
    let cancelled = false;
    completeSignIn().then(
      (session) => !cancelled && setState(toState(session)),
      (error: unknown) =>
        !cancelled &&
        setState({ status: "signedOut", message: error instanceof Error ? error.message : "Sign-in failed." }),
    );
    return () => {
      cancelled = true;
    };
  }, [state.status]);

  // The API answers 401 once the access token expires; sign out locally at the same moment.
  const expiresAt = state.status === "signedIn" ? state.expiresAt : null;
  useEffect(() => {
    if (expiresAt === null) return;
    const timer = setTimeout(() => {
      clearSession();
      setState({ status: "signedOut", message: "Your session expired. Please sign in again." });
    }, Math.max(0, expiresAt - Date.now()));
    return () => clearTimeout(timer);
  }, [expiresAt]);

  const signIn = useCallback(() => {
    startSignIn().catch(() =>
      setState({ status: "signedOut", message: "Could not start sign-in. Please try again." }),
    );
  }, []);
  const signOut = useCallback(() => signOutOfCognito(), []);
  const expireSession = useCallback((message = "Please sign in again.") => {
    clearSession();
    setState({ status: "signedOut", message });
  }, []);

  const value = useMemo(
    () => ({ state, signIn, signOut, expireSession }),
    [state, signIn, signOut, expireSession],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside <AuthProvider>");
  return value;
}
