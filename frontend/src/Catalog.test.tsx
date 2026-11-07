import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { ACCESS_TOKEN, CARDS, jsonResponse, signInForTest } from "./test-utils";

afterEach(() => vi.unstubAllGlobals());

describe("catalog", () => {
  it("shows the three cards with their quantities, fetched with the access token", async () => {
    signInForTest();
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ cards: CARDS })));
    vi.stubGlobal("fetch", fetchMock);

    render(<App />);

    const items = await screen.findAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(within(items[0]!).getByRole("heading", { name: "Ember Drake" })).toBeInTheDocument();
    expect(within(items[0]!).getByText("12 available")).toBeInTheDocument();
    expect(within(items[1]!).getByText("0 available")).toBeInTheDocument();
    expect(within(items[2]!).getByText("3 available")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Reserve" })).toHaveLength(3);

    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${ACCESS_TOKEN}`);
  });

  it("sends the trainer back to sign in when the API answers 401", async () => {
    signInForTest();
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse({ message: "Unauthorized" }, 401))));

    render(<App />);

    expect(await screen.findByRole("alert")).toHaveTextContent(/sign in again/i);
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(sessionStorage.getItem("pokedrop.session")).toBeNull();
  });

  it("explains a 403 as a token-type problem and keeps the trainer signed in", async () => {
    signInForTest();
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse({ message: "Forbidden" }, 403))));

    render(<App />);

    expect(await screen.findByRole("alert")).toHaveTextContent(/wrong token type/i);
    expect(screen.queryByRole("button", { name: "Sign in" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("offers a retry after a server error", async () => {
    signInForTest();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: { code: "INTERNAL_ERROR", message: "boom" } }, 500))
      .mockResolvedValueOnce(jsonResponse({ cards: CARDS }));
    vi.stubGlobal("fetch", fetchMock);

    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/server had a problem/i);

    await userEvent.click(screen.getByRole("button", { name: "Try again" }));

    expect(await screen.findAllByRole("listitem")).toHaveLength(3);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
