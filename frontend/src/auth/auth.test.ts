import { config } from "../config";
import {
  buildAuthorizeUrl,
  completeSignIn,
  loadSession,
  signOut,
  startSignIn,
  subFromToken,
} from "./auth";

function fakeJwt(payload: object): string {
  const encode = (value: object) => btoa(JSON.stringify(value)).replace(/=+$/, "");
  return `${encode({ alg: "none" })}.${encode(payload)}.sig`;
}

function tokenResponse(body: object, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(body), { status }));
}

/** Run startSignIn; returns the authorize-URL params plus the PKCE state/verifier it stored. */
async function beginSignIn() {
  const navigate = vi.fn();
  await startSignIn(navigate);
  const params = new URL(navigate.mock.calls[0]![0] as string).searchParams;
  const stored = JSON.parse(sessionStorage.getItem("pokedrop.pkce")!) as { state: string; verifier: string };
  return { params, ...stored };
}

// The in-flight guard is keyed by query string, so give every test a unique code.
let n = 0;
const nextCode = () => `code-${++n}`;

describe("startSignIn", () => {
  it("redirects to the hosted UI with an S256 code challenge and the required scope", async () => {
    const { params } = await beginSignIn();
    expect(params.get("response_type")).toBe("code");
    expect(params.get("client_id")).toBe(config.auth.clientId);
    expect(params.get("redirect_uri")).toBe("http://localhost:5173");
    expect(params.get("scope")).toBe("openid email aws.cognito.signin.user.admin");
    expect(params.get("code_challenge_method")).toBe("S256");
    expect(params.get("code_challenge")).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(params.get("state")).toBeTruthy();
  });

  it("targets the /oauth2/authorize endpoint of the hosted UI domain", () => {
    const url = new URL(buildAuthorizeUrl("s", "c"));
    expect(url.origin).toBe(config.auth.authority);
    expect(url.pathname).toBe("/oauth2/authorize");
  });
});

describe("completeSignIn", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("exchanges the code with the PKCE verifier, keeps only the access token and cleans the URL", async () => {
    const { state, verifier } = await beginSignIn();
    const fetchMock = vi.fn(() =>
      tokenResponse({ access_token: "access", id_token: "id", refresh_token: "refresh", expires_in: 3600 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const code = nextCode();
    window.history.replaceState({}, "", `/?code=${code}&state=${state}`);

    const session = await completeSignIn(window.location.search);

    expect(session.accessToken).toBe("access");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [URL, RequestInit];
    expect(url.toString()).toBe(`${config.auth.authority}/oauth2/token`);
    const body = new URLSearchParams(init.body as URLSearchParams);
    expect(Object.fromEntries(body)).toEqual({
      grant_type: "authorization_code",
      client_id: config.auth.clientId,
      code,
      redirect_uri: "http://localhost:5173",
      code_verifier: verifier,
    });
    expect(window.location.search).toBe("");
    expect(sessionStorage.getItem("pokedrop.pkce")).toBeNull();
    expect(loadSession()?.accessToken).toBe("access");
    expect(sessionStorage.getItem("pokedrop.session")).not.toMatch(/id_token|refresh/);
  });

  it("rejects a mismatched state without calling the token endpoint", async () => {
    await beginSignIn();
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", `/?code=${nextCode()}&state=forged`);

    await expect(completeSignIn(window.location.search)).rejects.toThrow(/did not match/);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(loadSession()).toBeNull();
    expect(window.location.search).toBe("");
  });

  it("rejects a callback when no sign-in was started in this tab", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    await expect(completeSignIn(`?code=${nextCode()}&state=x`)).rejects.toThrow(/did not match/);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("surfaces an error returned by the hosted UI", async () => {
    await expect(
      completeSignIn(`?error=access_denied&error_description=User+cancelled&n=${nextCode()}`),
    ).rejects.toThrow("User cancelled");
  });

  it("redeems the code once when called twice for the same URL (StrictMode)", async () => {
    const { state } = await beginSignIn();
    const fetchMock = vi.fn(() => tokenResponse({ access_token: "a", expires_in: 60 }));
    vi.stubGlobal("fetch", fetchMock);
    const search = `?code=${nextCode()}&state=${state}`;

    const [first, second] = await Promise.all([completeSignIn(search), completeSignIn(search)]);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(first).toBe(second);
  });

  it("reports a failed token exchange and stores no session", async () => {
    const { state } = await beginSignIn();
    vi.stubGlobal("fetch", vi.fn(() => tokenResponse({ error: "invalid_grant" }, 400)));

    await expect(completeSignIn(`?code=${nextCode()}&state=${state}`)).rejects.toThrow(/400/);
    expect(loadSession()).toBeNull();
  });

  it("reports an unreachable token endpoint", async () => {
    const { state } = await beginSignIn();
    vi.stubGlobal("fetch", vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))));

    await expect(completeSignIn(`?code=${nextCode()}&state=${state}`)).rejects.toThrow(/Could not reach/);
  });
});

describe("session", () => {
  it("ignores an expired session", () => {
    sessionStorage.setItem("pokedrop.session", JSON.stringify({ accessToken: "t", expiresAt: 1000 }));
    expect(loadSession(2000)).toBeNull();
    expect(sessionStorage.getItem("pokedrop.session")).toBeNull();
  });

  it("signOut clears the session and redirects to the Cognito logout endpoint", () => {
    sessionStorage.setItem("pokedrop.session", JSON.stringify({ accessToken: "t", expiresAt: Date.now() + 1e6 }));
    const navigate = vi.fn();
    signOut(navigate);
    const url = new URL(navigate.mock.calls[0]![0] as string);
    expect(url.pathname).toBe("/logout");
    expect(url.searchParams.get("client_id")).toBe(config.auth.clientId);
    expect(url.searchParams.get("logout_uri")).toBe("http://localhost:5173");
    expect(loadSession()).toBeNull();
  });
});

describe("subFromToken", () => {
  it("reads the sub claim", () => {
    expect(subFromToken(fakeJwt({ sub: "trainer-1" }))).toBe("trainer-1");
  });
  it("returns null for malformed tokens", () => {
    expect(subFromToken("not-a-jwt")).toBeNull();
    expect(subFromToken(fakeJwt({ nope: 1 }))).toBeNull();
  });
});
