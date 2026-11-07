import { addAccepted, loadReservations } from "./store";

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
