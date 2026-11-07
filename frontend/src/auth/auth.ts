import { config } from "../config";
import { challengeFor, randomString } from "./pkce";

const PKCE_KEY = "pokedrop.pkce";
const SESSION_KEY = "pokedrop.session";

/**
 * Only the access token is kept: it is what the API needs. The ID token is not stored, and the
 * refresh token is deliberately ignored (an expired session means signing in again).
 * sessionStorage keeps it for the tab only.
 */
export interface Session {
  accessToken: string;
  /** Epoch milliseconds. */
  expiresAt: number;
}

export class AuthError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AuthError";
  }
}

type Navigate = (url: string) => void;
const defaultNavigate: Navigate = (url) => window.location.assign(url);

function readJson(key: string): unknown {
  try {
    const raw = sessionStorage.getItem(key);
    return raw === null ? null : JSON.parse(raw);
  } catch {
    return null;
  }
}

function write(key: string, value: unknown): void {
  try {
    sessionStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Storage unavailable: sign-in will not survive a reload, but the tab keeps working.
  }
}

function remove(key: string): void {
  try {
    sessionStorage.removeItem(key);
  } catch {
    // ignore
  }
}

export function loadSession(now = Date.now()): Session | null {
  const value = readJson(SESSION_KEY);
  if (typeof value !== "object" || value === null) return null;
  const { accessToken, expiresAt } = value as Partial<Session>;
  if (typeof accessToken !== "string" || typeof expiresAt !== "number") return null;
  if (expiresAt <= now) {
    remove(SESSION_KEY);
    return null;
  }
  return { accessToken, expiresAt };
}

export function clearSession(): void {
  remove(SESSION_KEY);
}

export function buildAuthorizeUrl(state: string, challenge: string): string {
  const url = new URL("/oauth2/authorize", config.auth.authority);
  url.search = new URLSearchParams({
    response_type: "code",
    client_id: config.auth.clientId,
    redirect_uri: config.auth.redirectUri,
    scope: config.auth.scope,
    state,
    code_challenge: challenge,
    code_challenge_method: "S256",
  }).toString();
  return url.toString();
}

export async function startSignIn(navigate: Navigate = defaultNavigate): Promise<void> {
  const verifier = randomString(32);
  const state = randomString(16);
  write(PKCE_KEY, { verifier, state });
  navigate(buildAuthorizeUrl(state, await challengeFor(verifier)));
}

export function signOut(navigate: Navigate = defaultNavigate): void {
  clearSession();
  const url = new URL("/logout", config.auth.authority);
  url.search = new URLSearchParams({
    client_id: config.auth.clientId,
    logout_uri: config.auth.redirectUri,
  }).toString();
  navigate(url.toString());
}

/** True when the current URL is Cognito redirecting back with a code or an error. */
export function isAuthRedirect(search = window.location.search): boolean {
  const params = new URLSearchParams(search);
  return params.has("code") || params.has("error");
}

function loadPkce(): { verifier: string; state: string } | null {
  const value = readJson(PKCE_KEY);
  if (typeof value !== "object" || value === null) return null;
  const { verifier, state } = value as { verifier?: unknown; state?: unknown };
  return typeof verifier === "string" && typeof state === "string" ? { verifier, state } : null;
}

async function exchangeCode(params: URLSearchParams): Promise<Session> {
  const error = params.get("error");
  if (error) throw new AuthError(params.get("error_description") ?? error);

  const pkce = loadPkce();
  if (!pkce || params.get("state") !== pkce.state) {
    throw new AuthError("The sign-in response did not match this browser session. Please try again.");
  }
  const code = params.get("code");
  if (!code) throw new AuthError("The sign-in response had no authorization code.");

  let response: Response;
  try {
    response = await fetch(new URL("/oauth2/token", config.auth.authority), {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "authorization_code",
        client_id: config.auth.clientId,
        code,
        redirect_uri: config.auth.redirectUri,
        code_verifier: pkce.verifier,
      }),
    });
  } catch {
    throw new AuthError("Could not reach the sign-in service. Please try again.");
  }
  if (!response.ok) throw new AuthError(`Sign-in failed (token exchange returned ${response.status}).`);

  const body = (await response.json().catch(() => null)) as {
    access_token?: unknown;
    expires_in?: unknown;
  } | null;
  if (typeof body?.access_token !== "string" || typeof body.expires_in !== "number") {
    throw new AuthError("Sign-in failed (unexpected token response).");
  }
  const session: Session = {
    accessToken: body.access_token,
    expiresAt: Date.now() + body.expires_in * 1000,
  };
  write(SESSION_KEY, session);
  return session;
}

let inFlight: { search: string; promise: Promise<Session> } | null = null;

/**
 * Finish the authorization-code + PKCE exchange for the current URL.
 *
 * An authorization code can be redeemed only once, and React StrictMode runs effects twice in
 * development, so calls for the same URL share one promise. The PKCE material is discarded and
 * the code is stripped from the address bar whatever the outcome.
 */
export function completeSignIn(search = window.location.search): Promise<Session> {
  if (inFlight?.search === search) return inFlight.promise;
  const promise = exchangeCode(new URLSearchParams(search)).finally(() => {
    remove(PKCE_KEY);
    window.history.replaceState({}, "", window.location.pathname);
  });
  inFlight = { search, promise };
  return promise;
}

/** The `sub` claim of an access token, used only to namespace local storage (never for trust). */
export function subFromToken(token: string): string | null {
  try {
    const payload = token.split(".")[1];
    if (!payload) return null;
    const json = atob(payload.replace(/-/g, "+").replace(/_/g, "/"));
    const sub = (JSON.parse(json) as { sub?: unknown }).sub;
    return typeof sub === "string" && sub !== "" ? sub : null;
  } catch {
    return null;
  }
}
