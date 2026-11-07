import { addAccepted, clearPending, loadPending, loadReservations, savePending } from "./store";

const record = (requestId: string, cardId = "card-001") => ({
  requestId,
  cardId,
  submittedAt: "2026-11-07T12:00:00.000Z",
});

describe("reservation store", () => {
  it("persists accepted reservations per trainer", () => {
    addAccepted("trainer-1", record("a"));
    addAccepted("trainer-2", record("b"));
    expect(loadReservations("trainer-1").map((r) => r.requestId)).toEqual(["a"]);
    expect(loadReservations("trainer-2").map((r) => r.requestId)).toEqual(["b"]);
  });

  it("ignores a repeated requestId", () => {
    addAccepted("trainer-1", record("a"));
    addAccepted("trainer-1", record("a"));
    expect(loadReservations("trainer-1")).toHaveLength(1);
  });

  it("survives corrupt storage", () => {
    localStorage.setItem("pokedrop.reservations.trainer-1", "{not json");
    expect(loadReservations("trainer-1")).toEqual([]);
    localStorage.setItem("pokedrop.reservations.trainer-1", JSON.stringify([record("a"), { nope: 1 }, null]));
    expect(loadReservations("trainer-1").map((r) => r.requestId)).toEqual(["a"]);
  });

  it("keeps only the most recent records", () => {
    for (let i = 0; i < 105; i++) addAccepted("trainer-1", record(`id-${i}`));
    const stored = loadReservations("trainer-1");
    expect(stored).toHaveLength(100);
    expect(stored.at(-1)?.requestId).toBe("id-104");
  });
});

describe("pending store", () => {
  const WEEK = 7 * 24 * 60 * 60 * 1000;
  const pending = (cardId: string, requestId = `r-${cardId}`, savedAt = new Date().toISOString()) => ({
    requestId,
    cardId,
    savedAt,
  });

  it("round-trips per trainer and replaces an earlier entry for the same card", () => {
    savePending("trainer-1", pending("card-001", "first"));
    savePending("trainer-1", pending("card-001", "second"));
    savePending("trainer-1", pending("card-002"));
    savePending("trainer-2", pending("card-001", "other"));
    expect(loadPending("trainer-1", WEEK).map((p) => p.requestId)).toEqual(["second", "r-card-002"]);
    expect(loadPending("trainer-2", WEEK).map((p) => p.requestId)).toEqual(["other"]);
  });

  it("clears one card and removes the key when nothing is left", () => {
    savePending("trainer-1", pending("card-001"));
    clearPending("trainer-1", "card-001");
    expect(loadPending("trainer-1", WEEK)).toEqual([]);
    expect(localStorage.getItem("pokedrop.pending.trainer-1")).toBeNull();
  });

  it("drops entries older than the maximum age", () => {
    const now = Date.parse("2026-11-07T12:00:00Z");
    savePending("trainer-1", pending("card-001", "old", "2026-11-07T11:00:00Z"));
    savePending("trainer-1", pending("card-002", "new", "2026-11-07T11:59:00Z"));
    expect(loadPending("trainer-1", 10 * 60_000, now).map((p) => p.requestId)).toEqual(["new"]);
    expect(JSON.parse(localStorage.getItem("pokedrop.pending.trainer-1")!)).toHaveLength(1);
  });

  it("survives corrupt storage", () => {
    localStorage.setItem("pokedrop.pending.trainer-1", "{nope");
    expect(loadPending("trainer-1", WEEK)).toEqual([]);
    localStorage.setItem("pokedrop.pending.trainer-1", JSON.stringify([pending("card-001"), { bad: 1 }]));
    expect(loadPending("trainer-1", WEEK)).toHaveLength(1);
  });
});
