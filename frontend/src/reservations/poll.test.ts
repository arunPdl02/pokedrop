import { ApiError, type ReservationOutcome } from "../api/client";
import { abortableSleep, pollForOutcome } from "./poll";

const CONFIRMED: ReservationOutcome = { status: "CONFIRMED", updatedAt: "2026-11-07T20:15:30Z" };

/** A fake clock that only moves when the poller sleeps, so tests are deterministic. */
function fakeClock() {
  let t = 0;
  const waits: number[] = [];
  return {
    waits,
    now: () => t,
    sleep: (ms: number) => {
      waits.push(ms);
      t += ms;
      return Promise.resolve();
    },
  };
}

const signal = () => new AbortController().signal;

describe("pollForOutcome", () => {
  it("keeps polling through 'no outcome yet' with increasing intervals, capped at the last one", async () => {
    const clock = fakeClock();
    const check = vi.fn<() => Promise<ReservationOutcome | null>>().mockResolvedValue(null);
    check.mockResolvedValueOnce(null).mockResolvedValueOnce(null).mockResolvedValueOnce(null);
    check.mockResolvedValueOnce(null).mockResolvedValueOnce(null).mockResolvedValueOnce(CONFIRMED);

    const result = await pollForOutcome({
      check,
      intervalsMs: [1, 2, 3],
      windowMs: 1000,
      signal: signal(),
      ...clock,
    });

    expect(result).toEqual({ kind: "outcome", outcome: CONFIRMED });
    expect(clock.waits).toEqual([1, 2, 3, 3, 3, 3]);
  });

  it("reports delayed, not an outcome, when the window runs out", async () => {
    const clock = fakeClock();
    const check = vi.fn(() => Promise.resolve(null));

    const result = await pollForOutcome({ check, intervalsMs: [4], windowMs: 10, signal: signal(), ...clock });

    expect(result).toEqual({ kind: "delayed" });
    expect(check).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["network", new ApiError("network", "offline")],
    ["throttled", new ApiError("throttled", "slow", 429)],
    ["server", new ApiError("server", "boom", 500, "INTERNAL_ERROR")],
    ["unreadable", new ApiError("unknown", "huh", 200)],
  ])("treats a %s error as 'not known yet' and keeps polling", async (_name, error) => {
    const clock = fakeClock();
    const check = vi
      .fn<() => Promise<ReservationOutcome | null>>()
      .mockRejectedValueOnce(error)
      .mockResolvedValueOnce(CONFIRMED);

    const result = await pollForOutcome({ check, intervalsMs: [1], windowMs: 100, signal: signal(), ...clock });

    expect(result).toEqual({ kind: "outcome", outcome: CONFIRMED });
    expect(check).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["unauthorized", 401],
    ["forbidden", 403],
  ] as const)("stops on %s, which polling cannot fix", async (kind, status) => {
    const clock = fakeClock();
    const check = vi.fn(() => Promise.reject(new ApiError(kind, "no", status)));
    await expect(
      pollForOutcome({ check, intervalsMs: [1], windowMs: 100, signal: signal(), ...clock }),
    ).rejects.toMatchObject({ kind });
    expect(check).toHaveBeenCalledTimes(1);
  });

  it("stops when aborted", async () => {
    const controller = new AbortController();
    const check = vi.fn(() => Promise.resolve(null));
    const polling = pollForOutcome({ check, intervalsMs: [60_000], windowMs: 120_000, signal: controller.signal });
    controller.abort();
    await expect(polling).rejects.toMatchObject({ name: "AbortError" });
    expect(check).not.toHaveBeenCalled();
  });
});

describe("abortableSleep", () => {
  it("rejects at once for an already-aborted signal", async () => {
    const controller = new AbortController();
    controller.abort();
    await expect(abortableSleep(1000, controller.signal)).rejects.toMatchObject({ name: "AbortError" });
  });
});
