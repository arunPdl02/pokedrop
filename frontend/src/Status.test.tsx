import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { ACCESS_TOKEN, CARDS, jsonResponse, signInForTest } from "./test-utils";

// Short real intervals; individual tests shrink the window to reach "Delayed".
const pollConfig = vi.hoisted(() => ({ intervalsMs: [5] as number[], windowMs: 5000 }));
vi.mock("./config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./config")>();
  return { config: { ...actual.config, submission: { retryDelaysMs: [0, 0, 0] }, polling: pollConfig } };
});

type StatusReply = Response | Error | Promise<Response>;
interface Context {
  /** 1-based number of this status call, across all requests. */
  call: number;
  requestId: string;
  cardId: string;
}

const notYet = () => jsonResponse({ error: { code: "RESERVATION_NOT_FOUND", message: "not found" } }, 404);
const outcome = (status: "CONFIRMED" | "SOLD_OUT", requestId = "r", cardId = "card-001") =>
  jsonResponse({ requestId, cardId, status, updatedAt: "2026-11-07T20:15:30Z" });

function stubApi(statusFor: (context: Context) => StatusReply) {
  const posts: { requestId: string; cardId: string }[] = [];
  const statusCalls: { path: string; authorization: string }[] = [];
  const cardOf = new Map<string, string>();
  let cardsCalls = 0;

  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      if (url.endsWith("/cards")) {
        cardsCalls++;
        return Promise.resolve(jsonResponse({ cards: CARDS }));
      }
      if (url.endsWith("/reservations") && init?.method === "POST") {
        const body = JSON.parse(init.body as string) as { requestId: string; cardId: string };
        posts.push(body);
        cardOf.set(body.requestId, body.cardId);
        return Promise.resolve(jsonResponse({ requestId: body.requestId, status: "QUEUED" }, 202));
      }
      const match = /\/reservations\/([^/]+)$/.exec(url);
      if (match) {
        const requestId = decodeURIComponent(match[1]!);
        statusCalls.push({
          path: new URL(url).pathname,
          authorization: (init?.headers as Record<string, string>).Authorization!,
        });
        const reply = statusFor({ call: statusCalls.length, requestId, cardId: cardOf.get(requestId) ?? "" });
        return reply instanceof Error ? Promise.reject(reply) : Promise.resolve(reply);
      }
      throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url}`);
    }),
  );
  return { posts, statusCalls, cardsCalls: () => cardsCalls };
}

async function renderCatalog() {
  signInForTest();
  const view = render(<App />);
  await screen.findAllByRole("listitem");
  return view;
}

const card = (index: number) => screen.getAllByRole("listitem")[index]!;
const reserveButton = (index: number) => within(card(index)).getByRole("button", { name: /^reserve$/i });
const seed = (records: object[]) =>
  localStorage.setItem("pokedrop.reservations.trainer-1", JSON.stringify(records));
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

beforeEach(() => {
  pollConfig.intervalsMs = [5];
  pollConfig.windowMs = 5000;
});
afterEach(() => vi.unstubAllGlobals());

describe("reservation status", () => {
  it("shows Queued while the API answers 404, then Confirmed; persists it and refreshes stock", async () => {
    let release!: (response: Response) => void;
    const gate = new Promise<Response>((resolve) => (release = resolve));
    const api = stubApi(({ call, requestId }) => {
      if (call === 1) return gate;
      if (call <= 3) return notYet();
      return outcome("CONFIRMED", requestId);
    });
    await renderCatalog();
    await userEvent.click(reserveButton(0));

    // The first check is still in flight: the 202 only means queued.
    expect(await within(card(0)).findByText("Queued")).toBeInTheDocument();
    expect(within(card(0)).queryByText("Confirmed")).not.toBeInTheDocument();
    release(notYet());

    expect(await within(card(0)).findByText("Confirmed")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(api.statusCalls.length).toBeGreaterThanOrEqual(4);
    for (const call of api.statusCalls) {
      expect(call.path).toBe(`/reservations/${api.posts[0]!.requestId}`);
      expect(call.authorization).toBe(`Bearer ${ACCESS_TOKEN}`);
    }
    const stored = JSON.parse(localStorage.getItem("pokedrop.reservations.trainer-1")!) as {
      outcome: { status: string };
    }[];
    expect(stored[0]!.outcome.status).toBe("CONFIRMED");
    await waitFor(() => expect(api.cardsCalls()).toBe(2));

    // Polling stops once the outcome is known.
    const settled = api.statusCalls.length;
    await sleep(40);
    expect(api.statusCalls).toHaveLength(settled);
  });

  it("shows Sold out when the worker recorded insufficient stock", async () => {
    stubApi(({ requestId }) => outcome("SOLD_OUT", requestId));
    await renderCatalog();
    await userEvent.click(reserveButton(1));
    expect(await within(card(1)).findByText("Sold out")).toBeInTheDocument();
    expect(within(card(1)).queryByText("Confirmed")).not.toBeInTheDocument();
  });

  it("lets the trainer reserve again after an outcome, as a new request", async () => {
    const api = stubApi(({ requestId }) => outcome("CONFIRMED", requestId));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Confirmed");

    await userEvent.click(reserveButton(0));
    await waitFor(() => expect(api.posts).toHaveLength(2));
    expect(api.posts[1]!.requestId).not.toBe(api.posts[0]!.requestId);
  });

  it("keeps cards independent", async () => {
    stubApi(({ requestId, cardId }) => outcome(cardId === "card-001" ? "CONFIRMED" : "SOLD_OUT", requestId, cardId));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await userEvent.click(reserveButton(2));
    expect(await within(card(0)).findByText("Confirmed")).toBeInTheDocument();
    expect(await within(card(2)).findByText("Sold out")).toBeInTheDocument();
    expect(within(card(1)).queryByRole("status")).not.toBeInTheDocument();
  });

  it("treats network, throttling and server errors while polling as 'not yet', never as an outcome", async () => {
    stubApi(({ call, requestId }) => {
      if (call === 1) return new TypeError("Failed to fetch");
      if (call === 2) return jsonResponse({ message: "Too Many Requests" }, 429);
      if (call === 3) return jsonResponse({ error: { code: "INTERNAL_ERROR", message: "boom" } }, 500);
      return outcome("CONFIRMED", requestId);
    });
    await renderCatalog();
    await userEvent.click(reserveButton(0));

    expect(await within(card(0)).findByText("Confirmed")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(within(card(0)).queryByText("Sold out")).not.toBeInTheDocument();
    expect(within(card(0)).queryByText("Delayed")).not.toBeInTheDocument();
  });
});

describe("delayed", () => {
  it("shows Delayed when the window runs out, never Sold out, and stops polling", async () => {
    pollConfig.windowMs = 40;
    const api = stubApi(() => notYet());
    await renderCatalog();
    await userEvent.click(reserveButton(0));

    expect(await within(card(0)).findByText("Delayed")).toBeInTheDocument();
    expect(within(card(0)).queryByText("Sold out")).not.toBeInTheDocument();
    expect(within(card(0)).queryByText("Confirmed")).not.toBeInTheDocument();
    expect(within(card(0)).getByText(/not been lost/i)).toBeInTheDocument();
    // The request may still complete, so no second request for the same card.
    expect(within(card(0)).getByRole("button", { name: "Reserve" })).toBeDisabled();
    expect(api.posts).toHaveLength(1);
    // Nothing was written as an outcome.
    expect(localStorage.getItem("pokedrop.reservations.trainer-1")).not.toMatch(/outcome/);

    const stopped = api.statusCalls.length;
    await sleep(60);
    expect(api.statusCalls).toHaveLength(stopped);
  });

  it("Check again polls with a fresh window and shows the outcome when it arrives", async () => {
    pollConfig.windowMs = 40;
    let late = false;
    stubApi(({ requestId }) => (late ? outcome("CONFIRMED", requestId) : notYet()));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Delayed");

    late = true;
    pollConfig.windowMs = 5000;
    await userEvent.click(within(card(0)).getByRole("button", { name: "Check again" }));

    expect(await within(card(0)).findByText("Confirmed")).toBeInTheDocument();
    expect(within(card(0)).queryByText("Delayed")).not.toBeInTheDocument();
  });

  it("shows a 403 while polling as a problem on the card, not as an outcome", async () => {
    stubApi(() => jsonResponse({ message: "Forbidden" }, 403));
    await renderCatalog();
    await userEvent.click(reserveButton(0));

    expect(await within(card(0)).findByText("Delayed")).toBeInTheDocument();
    expect(within(card(0)).getByText(/wrong token type/i)).toBeInTheDocument();
    expect(within(card(0)).queryByText("Sold out")).not.toBeInTheDocument();
    expect(within(card(0)).getByRole("button", { name: "Check again" })).toBeInTheDocument();
  });

  it("sends the trainer back to sign in when a status check answers 401", async () => {
    stubApi(() => jsonResponse({ message: "Unauthorized" }, 401));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    expect(await screen.findByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(/sign in again/i);
  });
});

describe("after a refresh", () => {
  it("resumes polling a stored queued request without submitting anything", async () => {
    const stored = { requestId: "seeded-1", cardId: "card-001", submittedAt: "2026-11-07T12:00:00.000Z" };
    seed([stored]);
    const api = stubApi(({ call, requestId }) => (call < 3 ? notYet() : outcome("CONFIRMED", requestId)));
    await renderCatalog();

    expect(within(card(0)).getByText("Queued")).toBeInTheDocument();
    expect(await within(card(0)).findByText("Confirmed")).toBeInTheDocument();
    expect(api.statusCalls[0]!.path).toBe("/reservations/seeded-1");
    expect(api.posts).toHaveLength(0);
  });

  it("shows a stored outcome straight away and does not poll it again", async () => {
    seed([
      {
        requestId: "seeded-2",
        cardId: "card-002",
        submittedAt: "2026-11-07T12:00:00.000Z",
        outcome: { status: "SOLD_OUT", updatedAt: "2026-11-07T12:00:05Z" },
      },
    ]);
    const api = stubApi(() => notYet());
    await renderCatalog();
    expect(within(card(1)).getByText("Sold out")).toBeInTheDocument();
    await sleep(40);
    expect(api.statusCalls).toHaveLength(0);
  });
});
