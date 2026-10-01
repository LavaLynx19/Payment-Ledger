// Shared k6 helpers: the gRPC client, authenticated idempotent calls, and
// funding Wallets before a load phase.
import grpc from 'k6/net/grpc';
import { sleep } from 'k6';

const TARGET = __ENV.TARGET || 'api:8080';
const TOKEN = __ENV.LEDGER_TOKEN || 'dev-token';

export const client = new grpc.Client();
client.load(['/proto'], 'ledger/v1/ledger.proto');

export const wallets = JSON.parse(open('/out/seed.json')).wallet_ids;

export const pick = (list) => list[Math.floor(Math.random() * list.length)];

export function connect() {
  client.connect(TARGET, { plaintext: true });
}

const newKey = () => `k6-${Date.now()}-${Math.random().toString(36).slice(2)}`;

function invoke(method, body, key) {
  return client.invoke(`ledger.v1.LedgerService/${method}`, body, {
    metadata: { authorization: `Bearer ${TOKEN}`, 'idempotency-key': key },
    timeout: '5s',
  });
}

// Statuses that are a definite answer. Anything else (a crash surfacing as
// UNAVAILABLE or a thrown connection error, a restarting database as INTERNAL,
// ABORTED contention) leaves the outcome unknown, so the client retries with
// the same key. This is an allowlist of definite answers, so an unknown status
// gets the safe retry.
//
// k6 v2 exposes gRPC statuses as objects, and a response's status isn't the
// same object as the grpc.Status* constant, so Set membership by identity
// never matches. Compare numeric codes (Number(status)) instead. The first
// Rung 2 matrix counted UNAVAILABLE as failed for exactly this reason.
export const code = (status) => Number(status);
const DEFINITE = new Set([
  grpc.StatusOK, grpc.StatusFailedPrecondition, grpc.StatusInvalidArgument,
  grpc.StatusNotFound, grpc.StatusAlreadyExists, grpc.StatusUnauthenticated,
].map(code));

function reconnect() {
  try { client.close(); } catch (_) { /* already closed */ }
  try { connect(); } catch (_) { /* server still down; the next attempt retries */ }
}

// resilient keeps one Idempotency-Key across retries of a single request,
// reconnecting after crashes, for up to maxWaitMs. The window must stay well
// under the server's key retention, or a late retry would count as a new
// request. gaveUp is true when the outcome is still unknown at the deadline.
export function resilient(method, body, maxWaitMs = 10_000) {
  const key = newKey();
  const start = Date.now();
  let delay = 0.05;
  for (;;) {
    let res = null;
    try {
      res = invoke(method, body, key);
    } catch (_) {
      res = null; // connection broken by a crash
    }
    if (res && DEFINITE.has(code(res.status))) return { res, key, gaveUp: false };
    if (Date.now() - start > maxWaitMs) return { res, key, gaveUp: true };
    sleep(delay);
    delay = Math.min(delay * 2, 1);
    reconnect();
  }
}

// call sends one RPC with the service token and a fresh Idempotency-Key.
export function call(method, body) {
  return invoke(method, body, newKey());
}

// callRetrying follows the ABORTED contract (A§7): retry with the same
// Idempotency-Key, so a retry that races a success still moves money once.
export function callRetrying(method, body, attempts = 20) {
  const key = newKey();
  let res;
  for (let i = 0; i < attempts; i++) {
    res = invoke(method, body, key);
    if (code(res.status) !== code(grpc.StatusAborted)) return res;
    sleep(0.005 * (i + 1));
  }
  return res;
}

// fund tops up each Wallet by amount, one at a time, waiting for each to post
// before the next. Every TopUp debits the one funding account (a hot key, Rung
// 3). Firing them back to back makes each Accept's CAS race the capture of the
// previous one on that row, and setup failed with ABORTED even after 10s of
// retries. Serializing avoids the race, and resilient() also covers Rung 2
// failpoints crashing the server during setup. Call from setup().
export function fund(list, amount) {
  connect();
  const must = (method, body) => {
    const r = resilient(method, body);
    if (r.gaveUp || code(r.res.status) !== code(grpc.StatusOK)) {
      throw new Error(`${method} ${JSON.stringify(body)}: ${JSON.stringify(r.res && r.res.error)}`);
    }
    return r.res;
  };
  for (const w of list) {
    must('TopUp', { wallet_id: w, amount });
    const deadline = Date.now() + 30_000;
    while (Number(must('GetBalance', { account_id: w }).message.posted) !== amount) {
      if (Date.now() > deadline) throw new Error(`wallet ${w} never reached ${amount}`);
      sleep(0.01);
    }
  }
  client.close();
}
