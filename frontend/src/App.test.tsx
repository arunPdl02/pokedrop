import { render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { App } from "./App";
import { startSignIn } from "./auth/auth";
import { fakeJwt } from "./test-utils";

describe("App sign-in", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("offers sign-in when there is no session", () => {
    render(<App />);
    expect(screen.getByRole("heading", { name: "PokéDrop" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("completes the redirect once under StrictMode and shows the signed-in view", async () => {
    await startSignIn(() => {});
    const { state } = JSON.parse(sessionStorage.getItem("pokedrop.pkce")!) as { state: string };
    const accessToken = fakeJwt({ sub: "trainer-1" });
    const fetchMock = vi.fn((url: URL | string) =>
      Promise.resolve(
        String(url).endsWith("/oauth2/token")
          ? new Response(JSON.stringify({ access_token: accessToken, expires_in: 3600 }))
          : new Response(JSON.stringify({ cards: [] })),
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", `/?code=app-test-code&state=${state}`);

    render(
      <StrictMode>
        <App />
      </StrictMode>,
    );

    expect(await screen.findByRole("button", { name: "Sign out" })).toBeInTheDocument();
    const tokenCalls = fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/oauth2/token"));
    expect(tokenCalls).toHaveLength(1);
    expect(window.location.search).toBe("");
  });

  it("shows the error and a way to retry when the redirect carries a bad state", async () => {
    window.history.replaceState({}, "", "/?code=app-test-bad&state=forged");
    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/did not match/);
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });
});
