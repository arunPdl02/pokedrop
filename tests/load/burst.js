// k6 burst against POST /reservations (docs/benchmark.md).
//
// Every iteration submits one reservation with a fresh requestId and writes one JSON line to the
// console: the requestId and the moment it was submitted. Run it with --console-output so those
// lines land in a file, which `drainverify verify` reads later to compute time to final outcome.
// Use `make load`, which also runs the preflight, gets the token and sets the output file.
//
// Environment:
//   API_URL       base URL of the stack (the ApiUrl output), required
//   TOKEN         a Cognito access token for one trainer, required
//   CARD_ID       card to reserve, default card-001 (one card = the hot-key case)
//   REQUESTS      total number of requests, default 20
//   CONCURRENCY   virtual users, each with one request in flight, default 2
//   MAX_DURATION  give up after this long, default 5m
//
// Requests are not retried, so throttles and errors show up as they happened.
import http from 'k6/http';
import { check } from 'k6';

const API_URL = (__ENV.API_URL || '').replace(/\/+$/, '');
const TOKEN = __ENV.TOKEN || '';
const CARD_ID = __ENV.CARD_ID || 'card-001';
const REQUESTS = parseInt(__ENV.REQUESTS || '20', 10);
const CONCURRENCY = parseInt(__ENV.CONCURRENCY || '2', 10);

if (!API_URL || !TOKEN) {
  throw new Error('API_URL and TOKEN are required (use make load)');
}
if (!(REQUESTS > 0) || !(CONCURRENCY > 0)) {
  throw new Error('REQUESTS and CONCURRENCY must be positive integers');
}

export const options = {
  scenarios: {
    burst: {
      executor: 'shared-iterations',
      vus: CONCURRENCY,
      iterations: REQUESTS,
      maxDuration: __ENV.MAX_DURATION || '5m',
    },
  },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'max'],
};

const params = {
  headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
  tags: { name: 'POST /reservations' },
};

export default function () {
  const requestId = crypto.randomUUID();
  const body = JSON.stringify({ requestId, cardId: CARD_ID });

  const submittedAtMs = Date.now();
  const res = http.post(`${API_URL}/reservations`, body, params);
  const respondedAtMs = Date.now();

  // One line per request. status 0 means the request never got a response (see error).
  console.log(
    JSON.stringify({
      requestId,
      cardId: CARD_ID,
      submittedAtMs,
      respondedAtMs,
      status: res.status,
      durationMs: res.timings.duration,
      error: res.error || undefined,
    }),
  );
  check(res, { 'accepted (202)': (r) => r.status === 202 });
}
