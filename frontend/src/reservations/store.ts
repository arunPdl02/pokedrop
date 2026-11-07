/**
 * Accepted reservations (those the API answered 202 for), kept in localStorage so a refresh can
 * still check them. Keyed by the trainer's `sub` so trainers sharing a browser never see each
 * other's request ids; the API enforces ownership regardless.
 */
export interface StoredReservation {
  requestId: string;
  cardId: string;
  submittedAt: string;
  /** Set once the worker's terminal result has been read. Absent means still queued. */
  outcome?: StoredOutcome;
}

export interface StoredOutcome {
  status: "CONFIRMED" | "SOLD_OUT";
  updatedAt: string;
}

function isOutcome(value: unknown): value is StoredOutcome {
  if (typeof value !== "object" || value === null) return false;
  const outcome = value as Record<string, unknown>;
  return (outcome.status === "CONFIRMED" || outcome.status === "SOLD_OUT") && typeof outcome.updatedAt === "string";
}

const MAX_RECORDS = 100;

const keyFor = (sub: string) => `pokedrop.reservations.${sub}`;

function isStored(value: unknown): value is StoredReservation {
  if (typeof value !== "object" || value === null) return false;
  const record = value as Record<string, unknown>;
  return (
    typeof record.requestId === "string" &&
    typeof record.cardId === "string" &&
    typeof record.submittedAt === "string" &&
    (record.outcome === undefined || isOutcome(record.outcome))
  );
}

export function loadReservations(sub: string): StoredReservation[] {
  try {
    const raw = localStorage.getItem(keyFor(sub));
    const parsed: unknown = raw === null ? [] : JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter(isStored) : [];
  } catch {
    return [];
  }
}

function save(sub: string, records: StoredReservation[]): void {
  try {
    localStorage.setItem(keyFor(sub), JSON.stringify(records));
  } catch {
    // Storage full or blocked: the reservation is still queued server-side; only the
    // refresh-recovery convenience is lost.
  }
}

/** Add an accepted request. Re-adding a known requestId is a no-op. Returns the new list. */
export function addAccepted(sub: string, record: StoredReservation): StoredReservation[] {
  const current = loadReservations(sub);
  if (current.some((existing) => existing.requestId === record.requestId)) return current;
  const next = [...current, record].slice(-MAX_RECORDS);
  save(sub, next);
  return next;
}

/** Record the terminal outcome of a known request. Returns the new list. */
export function recordOutcome(sub: string, requestId: string, outcome: StoredOutcome): StoredReservation[] {
  const next = loadReservations(sub).map((record) =>
    record.requestId === requestId ? { ...record, outcome } : record,
  );
  save(sub, next);
  return next;
}

/**
 * A submission written BEFORE its POST is sent. If the page is closed or refreshed mid-request we
 * cannot know whether the server queued it, so the next load resends it with the same requestId;
 * the server is idempotent per requestId, so that can never reserve twice. It is removed once the
 * API accepts it (it then lives in the accepted list) or definitively rejects it.
 */
export interface PendingSubmission {
  requestId: string;
  cardId: string;
  savedAt: string;
}

const pendingKeyFor = (sub: string) => `pokedrop.pending.${sub}`;

function isPending(value: unknown): value is PendingSubmission {
  if (typeof value !== "object" || value === null) return false;
  const record = value as Record<string, unknown>;
  return (
    typeof record.requestId === "string" && typeof record.cardId === "string" && typeof record.savedAt === "string"
  );
}

function readPending(sub: string): PendingSubmission[] {
  try {
    const raw = localStorage.getItem(pendingKeyFor(sub));
    const parsed: unknown = raw === null ? [] : JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter(isPending) : [];
  } catch {
    return [];
  }
}

function writePending(sub: string, records: PendingSubmission[]): void {
  try {
    if (records.length === 0) localStorage.removeItem(pendingKeyFor(sub));
    else localStorage.setItem(pendingKeyFor(sub), JSON.stringify(records));
  } catch {
    // Best effort: without storage the submission still goes out, it just cannot be resumed.
  }
}

/** Pending submissions younger than `maxAgeMs`. Older ones are dropped from storage. */
export function loadPending(sub: string, maxAgeMs: number, now = Date.now()): PendingSubmission[] {
  const all = readPending(sub);
  const fresh = all.filter((record) => now - Date.parse(record.savedAt) <= maxAgeMs);
  if (fresh.length !== all.length) writePending(sub, fresh);
  return fresh;
}

/** Remember a submission about to be sent. At most one per card; saving again replaces it. */
export function savePending(sub: string, record: PendingSubmission): void {
  writePending(sub, [...readPending(sub).filter((r) => r.cardId !== record.cardId), record]);
}

export function clearPending(sub: string, cardId: string): void {
  writePending(
    sub,
    readPending(sub).filter((r) => r.cardId !== cardId),
  );
}
