import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode } from "react";
import { App } from "./App";
import { ACCESS_TOKEN, CARDS, jsonResponse, signInForTest } from "./test-utils";

// No real waiting between automatic retries in these tests.
vi.mock("./config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./config")>();
  return { config: { ...actual.config, submission: { ...actual.config.submission, retryDelaysMs: [0, 0, 0] } } };
});

type Posted = { requestId: string; cardId: string };
type PostReply = Response | Error | Promise<Response> | ((posted: Posted) => Response);

/** Called with each POST body just before the stub replies (to inspect storage at send time). */
let onPost: ((posted: Posted) => void) | undefined;

/** Routes GET /cards to the catalog and POST /reservations to the supplied replies, in order. */
function stubApi(...replies: PostReply[]) {
  const posts: Posted[] = [];
  const headers: HeadersInit[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (url.endsWith("/cards")) return Promise.resolve(jsonResponse({ cards: CARDS }));
    if (url.endsWith("/reservations") && init?.method === "POST") {
      const body = JSON.parse(init.body as string) as { requestId: string; cardId: string };
      posts.push(body);
      onPost?.(body);
      headers.push(init.headers as HeadersInit);
      const reply = replies[Math.min(posts.length, replies.length) - 1];
      if (reply === undefined) throw new Error("unexpected POST");
      if (reply instanceof Error) return Promise.reject(reply);
      return Promise.resolve(typeof reply === "function" ? reply(body) : reply);
    }
    // Status polling is covered in Status.test.tsx; here the request just has no outcome yet.
    if (url.includes("/reservations/")) return Promise.resolve(failure(404, "RESERVATION_NOT_FOUND"));
    throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url}`);
  });
  vi.stubGlobal("fetch", fetchMock);
  return { posts, headers };
}

/** 202 echoing the posted requestId, or a fixed one (the real API echoes it lowercased). */
const accepted =
  (echo?: string) =>
  (posted: Posted): Response =>
    jsonResponse({ requestId: echo ?? posted.requestId, status: "QUEUED" }, 202);
const failure = (status: number, code: string, message = "x") => jsonResponse({ error: { code, message } }, status);

async function renderCatalog() {
  signInForTest();
  const view = render(<App />);
  await screen.findAllByRole("listitem");
  return view;
}

const card = (index: number) => screen.getAllByRole("listitem")[index]!;
const reserveButton = (index: number) => within(card(index)).getByRole("button", { name: /reserve|try again|reserving|retrying/i });

afterEach(() => {
  vi.unstubAllGlobals();
  onPost = undefined;
});

describe("submitting a reservation", () => {
  it("posts one requestId and cardId with the access token and shows Queued, not reserved", async () => {
    const { posts, headers } = stubApi(accepted());
    await renderCatalog();

    await userEvent.click(reserveButton(0));

    expect(await within(card(0)).findByText("Queued")).toBeInTheDocument();
    expect(posts).toHaveLength(1);
    expect(posts[0]!.cardId).toBe("card-001");
    expect(posts[0]!.requestId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    expect(headers[0]).toMatchObject({ Authorization: `Bearer ${ACCESS_TOKEN}` });
    expect(within(card(0)).queryByText(/confirmed/i)).not.toBeInTheDocument();
    expect(reserveButton(0)).toBeDisabled();
    // The other cards are unaffected.
    expect(reserveButton(1)).toBeEnabled();
  });

  it("stores the accepted request id for this trainer so a refresh can find it", async () => {
    const { posts } = stubApi(accepted());
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");

    const stored = JSON.parse(localStorage.getItem("pokedrop.reservations.trainer-1")!) as { requestId: string; cardId: string }[];
    expect(stored).toHaveLength(1);
    expect(stored[0]).toMatchObject({ requestId: posts[0]!.requestId, cardId: "card-001" });
  });

  it("stores the request id the API echoes back (it lowercases ids)", async () => {
    stubApi(accepted("echoed-by-api"));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");
    const stored = JSON.parse(localStorage.getItem("pokedrop.reservations.trainer-1")!) as { requestId: string }[];
    expect(stored[0]!.requestId).toBe("echoed-by-api");
  });

  it("shows a queued reservation again after a reload without sending anything", async () => {
    stubApi(accepted());
    const first = await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");
    first.unmount();

    const { posts } = stubApi();
    render(<App />);
    await screen.findAllByRole("listitem");

    expect(within(card(0)).getByText("Queued")).toBeInTheDocument();
    expect(reserveButton(0)).toBeDisabled();
    expect(posts).toHaveLength(0);
  });

  it("does not show another trainer's stored reservations", async () => {
    localStorage.setItem(
      "pokedrop.reservations.someone-else",
      JSON.stringify([{ requestId: "x", cardId: "card-001", submittedAt: "2026-11-07T12:00:00.000Z" }]),
    );
    stubApi();
    await renderCatalog();
    expect(within(card(0)).queryByText("Queued")).not.toBeInTheDocument();
  });
});

describe("double clicks", () => {
  it("sends one request when the button is clicked twice before the first response", async () => {
    let respond!: (response: Response) => void;
    const { posts } = stubApi(new Promise<Response>((resolve) => (respond = resolve)));
    await renderCatalog();
    const button = reserveButton(0);

    // Two clicks in the same tick: React has not re-rendered, so `disabled` cannot help here.
    act(() => {
      fireEvent.click(button);
      fireEvent.click(button);
    });
    expect(posts).toHaveLength(1);
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("aria-busy", "true");

    await act(async () => respond(accepted()({ requestId: "r", cardId: "card-001" })));
    await within(card(0)).findByText("Queued");
    await userEvent.click(reserveButton(0));
    expect(posts).toHaveLength(1);
  });

  it("sends one request for a real double click", async () => {
    const { posts } = stubApi(accepted());
    await renderCatalog();
    await userEvent.dblClick(reserveButton(0));
    await within(card(0)).findByText("Queued");
    expect(posts).toHaveLength(1);
  });

  it("allows different cards to be submitted independently", async () => {
    const { posts } = stubApi(accepted(), accepted());
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await userEvent.click(reserveButton(2));
    await waitFor(() => expect(posts).toHaveLength(2));
    expect(posts.map((p) => p.cardId)).toEqual(["card-001", "card-003"]);
    expect(posts[0]!.requestId).not.toBe(posts[1]!.requestId);
  });
});

describe("retries reuse one request id", () => {
  it("retries a network failure with the same requestId", async () => {
    const { posts } = stubApi(new TypeError("Failed to fetch"), accepted());
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");
    expect(posts).toHaveLength(2);
    expect(posts[1]!.requestId).toBe(posts[0]!.requestId);
  });

  it.each([
    ["503 QUEUE_UNAVAILABLE", () => failure(503, "QUEUE_UNAVAILABLE")],
    ["500 INTERNAL_ERROR", () => failure(500, "INTERNAL_ERROR")],
    ["429 throttling", () => jsonResponse({ message: "Too Many Requests" }, 429)],
  ])("retries %s with the same requestId", async (_name, makeFailure) => {
    const { posts } = stubApi(makeFailure(), makeFailure(), accepted());
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");
    expect(posts).toHaveLength(3);
    expect(new Set(posts.map((p) => p.requestId)).size).toBe(1);
  });

  it("after automatic retries run out, the Try again click resends the same requestId", async () => {
    const down = () => failure(503, "QUEUE_UNAVAILABLE");
    const { posts } = stubApi(down(), down(), down(), down(), accepted());
    await renderCatalog();

    await userEvent.click(reserveButton(0));
    expect(await within(card(0)).findByRole("alert")).toHaveTextContent(/server had a problem/i);
    expect(posts).toHaveLength(4);
    expect(within(card(0)).queryByText("Queued")).not.toBeInTheDocument();

    await userEvent.click(within(card(0)).getByRole("button", { name: "Try again" }));
    await within(card(0)).findByText("Queued");

    expect(posts).toHaveLength(5);
    expect(new Set(posts.map((p) => p.requestId)).size).toBe(1);
  });
});

describe("API errors", () => {
  it("shows a rejected request without retrying and starts a fresh submission next time", async () => {
    const { posts } = stubApi(failure(400, "INVALID_REQUEST", "cardId is required"), accepted());
    await renderCatalog();

    await userEvent.click(reserveButton(0));
    expect(await within(card(0)).findByRole("alert")).toHaveTextContent("cardId is required");
    expect(posts).toHaveLength(1);
    expect(reserveButton(0)).toHaveTextContent("Reserve");

    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");
    expect(posts).toHaveLength(2);
    expect(posts[1]!.requestId).not.toBe(posts[0]!.requestId);
  });

  it("explains a card that is not in the catalog", async () => {
    const { posts } = stubApi(failure(404, "CARD_NOT_FOUND"));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    expect(await within(card(0)).findByRole("alert")).toHaveTextContent(/no longer in the catalog/i);
    expect(posts).toHaveLength(1);
    expect(localStorage.getItem("pokedrop.reservations.trainer-1")).toBeNull();
  });

  it("sends the trainer back to sign in on 401 and stores nothing", async () => {
    const { posts } = stubApi(jsonResponse({ message: "Unauthorized" }, 401));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    expect(await screen.findByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(/sign in again/i);
    expect(posts).toHaveLength(1);
    expect(localStorage.getItem("pokedrop.reservations.trainer-1")).toBeNull();
  });

  it("reports a 403 as a wrong-token-type problem without retrying", async () => {
    const { posts } = stubApi(jsonResponse({ message: "Forbidden" }, 403));
    await renderCatalog();
    await userEvent.click(reserveButton(0));
    expect(await within(card(0)).findByRole("alert")).toHaveTextContent(/wrong token type/i);
    expect(posts).toHaveLength(1);
  });
});

describe("pending submissions survive a refresh", () => {
  const PENDING_KEY = "pokedrop.pending.trainer-1";
  const ACCEPTED_KEY = "pokedrop.reservations.trainer-1";
  const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
  const seedPending = (records: object[], sub = "trainer-1") =>
    localStorage.setItem(`pokedrop.pending.${sub}`, JSON.stringify(records));
  const pendingRecord = (requestId: string, cardId = "card-001", savedAt = minutesAgo(1)) => ({
    requestId,
    cardId,
    savedAt,
  });
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

  /** Load the page as a returning trainer, without clicking anything. */
  async function reload(ui = <App />) {
    signInForTest();
    const view = render(ui);
    await screen.findAllByRole("listitem");
    return view;
  }

  it("saves the request id before the POST is sent and clears it once the API accepts it", async () => {
    let storedAtSendTime: unknown;
    onPost = () => (storedAtSendTime = JSON.parse(localStorage.getItem(PENDING_KEY) ?? "null"));
    const { posts } = stubApi(accepted());
    await renderCatalog();

    await userEvent.click(reserveButton(0));
    await within(card(0)).findByText("Queued");

    expect(storedAtSendTime).toEqual([
      { requestId: posts[0]!.requestId, cardId: "card-001", savedAt: expect.any(String) },
    ]);
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(JSON.parse(localStorage.getItem(ACCEPTED_KEY)!)).toHaveLength(1);
  });

  it("resends a submission interrupted by a refresh, with the same requestId", async () => {
    // The first POST never gets a response: the page is "refreshed" while it is in flight.
    const first = stubApi(new Promise<Response>(() => {}));
    const firstView = await renderCatalog();
    await userEvent.click(reserveButton(0));
    await waitFor(() => expect(first.posts).toHaveLength(1));
    const requestId = first.posts[0]!.requestId;
    expect(JSON.parse(localStorage.getItem(PENDING_KEY)!)).toMatchObject([{ requestId, cardId: "card-001" }]);
    firstView.unmount();

    const second = stubApi(accepted());
    await reload();

    // No click: the page itself resends, and it must be the same id.
    expect(await within(card(0)).findByText("Queued")).toBeInTheDocument();
    expect(second.posts).toEqual([{ requestId, cardId: "card-001" }]);
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(JSON.parse(localStorage.getItem(ACCEPTED_KEY)!)).toMatchObject([{ requestId }]);
  });

  it("resends after automatic retries ran out and the page was then closed", async () => {
    const down = () => failure(503, "QUEUE_UNAVAILABLE");
    const first = stubApi(down(), down(), down(), down());
    const firstView = await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByRole("alert");
    firstView.unmount();

    const second = stubApi(accepted());
    await reload();

    await within(card(0)).findByText("Queued");
    expect(second.posts).toHaveLength(1);
    expect(second.posts[0]!.requestId).toBe(first.posts[0]!.requestId);
  });

  it("keeps the id when a resend also fails, so Try again still reuses it", async () => {
    seedPending([pendingRecord("seeded-pending")]);
    const down = () => failure(503, "QUEUE_UNAVAILABLE");
    const { posts } = stubApi(down(), down(), down(), down(), accepted());
    await reload();

    expect(await within(card(0)).findByRole("alert")).toBeInTheDocument();
    expect(posts).toHaveLength(4);
    expect(localStorage.getItem(PENDING_KEY)).not.toBeNull();

    await userEvent.click(within(card(0)).getByRole("button", { name: "Try again" }));
    await within(card(0)).findByText("Queued");
    expect(new Set(posts.map((p) => p.requestId))).toEqual(new Set(["seeded-pending"]));
  });

  it("does not resend a request the API rejected", async () => {
    const first = stubApi(failure(400, "INVALID_REQUEST", "cardId is required"));
    const firstView = await renderCatalog();
    await userEvent.click(reserveButton(0));
    await within(card(0)).findByRole("alert");
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(first.posts).toHaveLength(1);
    firstView.unmount();

    const second = stubApi();
    await reload();
    await sleep(30);
    expect(second.posts).toHaveLength(0);
  });

  it("does not resend an id that was already accepted", async () => {
    seedPending([pendingRecord("already-accepted")]);
    localStorage.setItem(
      ACCEPTED_KEY,
      JSON.stringify([{ requestId: "already-accepted", cardId: "card-001", submittedAt: minutesAgo(1) }]),
    );
    const { posts } = stubApi();
    await reload();
    await sleep(30);
    expect(posts).toHaveLength(0);
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(within(card(0)).getByText("Queued")).toBeInTheDocument();
  });

  it("drops a pending submission that is too old instead of sending it", async () => {
    seedPending([pendingRecord("stale", "card-001", minutesAgo(11))]);
    const { posts } = stubApi();
    await reload();
    await sleep(30);
    expect(posts).toHaveLength(0);
    expect(localStorage.getItem(PENDING_KEY)).toBeNull();
    expect(reserveButton(0)).toBeEnabled();
  });

  it("ignores another trainer's pending submissions", async () => {
    seedPending([pendingRecord("someone-elses")], "someone-else");
    const { posts } = stubApi();
    await reload();
    await sleep(30);
    expect(posts).toHaveLength(0);
  });

  it("resends once, not twice, under StrictMode", async () => {
    seedPending([pendingRecord("strict-1")]);
    const { posts } = stubApi(accepted());
    await reload(
      <StrictMode>
        <App />
      </StrictMode>,
    );
    await within(card(0)).findByText("Queued");
    expect(posts).toEqual([{ requestId: "strict-1", cardId: "card-001" }]);
  });
});
