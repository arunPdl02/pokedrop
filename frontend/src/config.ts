// Deployment values come from /config.json, fetched once before the app renders (see main.tsx).
// The deploy writes it for each stage (CDK, BucketDeployment) and `make frontend-config` writes it
// for `npm run dev`, so one build works for every stage and nothing here depends on stack outputs.
// The values are identifiers, not secrets. Deliberately not read from .env files.

/** What config.json holds. Everything else in `config` is the same for every stage. */
export interface RuntimeConfig {
  /** API Gateway base URL, no trailing slash needed. */
  apiUrl: string;
  /** Client ID of the PKCE app client (not the dev-only client used by `make token`). */
  clientId: string;
  /** Hosted UI host name, without a scheme, e.g. `pokedrop-prod-abc.auth.us-west-2.amazoncognito.com`. */
  hostedUiDomain: string;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

/** Filled by applyConfig before the first render. Empty until then. */
export const config = {
  apiUrl: "",
  auth: {
    clientId: "",
    authority: "",
    // Must match the registered callback and logout URL exactly (no path, no trailing slash). Each
    // stage registers the origin it is served from, so this is always the current origin.
    redirectUri: "",
    // aws.cognito.signin.user.admin is required by the API's JWT authorizer.
    scope: "openid email aws.cognito.signin.user.admin",
  },
  polling: {
    // Waits before each status check; the last value repeats. Bounded by windowMs, after which the
    // UI shows "Delayed" (never "Sold out") and offers a manual re-check.
    intervalsMs: [1000, 2000, 3000, 5000, 8000, 10000],
    windowMs: 60_000,
  },
  submission: {
    // Waits between automatic retries of one submission; every attempt reuses the same requestId.
    retryDelaysMs: [500, 1000, 2000],
    // A submission whose outcome is unknown (page closed mid-POST, retries exhausted) is resent with
    // the same requestId on the next load, but only this long: it is safe to resend at any age,
    // yet a days-old click should not silently turn into a reservation.
    pendingMaxAgeMs: 10 * 60 * 1000,
  },
};

function requiredString(raw: Record<string, unknown>, key: keyof RuntimeConfig): string {
  const value = raw[key];
  if (typeof value !== "string" || value.trim() === "") {
    throw new ConfigError(`config.json is missing "${key}".`);
  }
  return value.trim();
}

/** Checks a parsed config.json and returns it normalized. Throws ConfigError if anything is off. */
export function parseConfig(raw: unknown): RuntimeConfig {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new ConfigError("config.json is not a JSON object.");
  }
  const fields = raw as Record<string, unknown>;

  const apiUrl = requiredString(fields, "apiUrl").replace(/\/+$/, "");
  let parsed: URL;
  try {
    parsed = new URL(apiUrl);
  } catch {
    throw new ConfigError('config.json "apiUrl" is not a URL.');
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
    throw new ConfigError('config.json "apiUrl" must be an http(s) URL.');
  }

  const hostedUiDomain = requiredString(fields, "hostedUiDomain");
  if (!/^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/i.test(hostedUiDomain)) {
    throw new ConfigError('config.json "hostedUiDomain" must be a host name without a scheme or path.');
  }

  return { apiUrl, clientId: requiredString(fields, "clientId"), hostedUiDomain };
}

/** Copies deployment values into `config`. Call once before the app renders. */
export function applyConfig(runtime: RuntimeConfig, origin: string = window.location.origin): void {
  config.apiUrl = runtime.apiUrl;
  config.auth.clientId = runtime.clientId;
  config.auth.authority = `https://${runtime.hostedUiDomain}`;
  config.auth.redirectUri = origin;
}

/**
 * Fetches /config.json (never from the browser cache, so a redeploy takes effect at once),
 * validates it and applies it.
 */
export async function loadConfig(fetchFn: typeof fetch = fetch): Promise<void> {
  let response: Response;
  try {
    response = await fetchFn("/config.json", { cache: "no-store" });
  } catch {
    throw new ConfigError("Could not load config.json. Check your connection and reload.");
  }
  if (!response.ok) {
    throw new ConfigError(`Could not load config.json (the server returned ${response.status}).`);
  }
  let raw: unknown;
  try {
    raw = await response.json();
  } catch {
    throw new ConfigError("config.json is not valid JSON.");
  }
  applyConfig(parseConfig(raw));
}
