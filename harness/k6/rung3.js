// Rung 3: hot Account contention at a fixed arrival RATE. Two hot keys, each
// with its own knob, so their costs can be measured separately:
//   HOT_DEST_SHARE  share of P2P Transfers paid to one "merchant" Wallet
//   TOPUP_SHARE     share of all operations that are TopUps, which all debit
//                   the single funding System account
// Requests retry with one Idempotency-Key on non-definite answers (ABORTED
// included). The CAS counters printed by run.sh show where conflicts land.
import grpc from 'k6/net/grpc';
import { Counter } from 'k6/metrics';
import { code, connect, fund, pick, resilient, wallets } from './lib.js';

const FUNDING = Number(__ENV.FUNDING || 1_000_000_000);
const RATE = Number(__ENV.RATE || 2000);
const HOT_DEST_SHARE = Number(__ENV.HOT_DEST_SHARE ?? 0.8);
const TOPUP_SHARE = Number(__ENV.TOPUP_SHARE ?? 0.2);

const merchant = wallets[0]; // the hot destination; it never sends
const payers = wallets.slice(1);

const ok = new Counter('ops_ok');
const rejected = new Counter('ops_rejected_expected');
const failed = new Counter('ops_failed');
const gaveUp = new Counter('ops_gave_up');

export const options = {
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: Number(__ENV.VUS || 200),
      maxVUs: Number(__ENV.MAX_VUS || 1000),
    },
  },
  thresholds: { ops_failed: ['count==0'], ops_gave_up: ['count==0'] },
  setupTimeout: '180s',
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  fund(payers, FUNDING);
}

function op(method, body) {
  const r = resilient(method, body);
  if (r.gaveUp) {
    gaveUp.add(1, { op: method });
    console.error(`${method}: gave up with outcome unknown (key ${r.key})`);
  } else if (code(r.res.status) === code(grpc.StatusOK)) {
    ok.add(1, { op: method });
  } else if (code(r.res.status) === code(grpc.StatusFailedPrecondition)) {
    rejected.add(1, { op: method });
  } else {
    failed.add(1, { op: method });
    console.error(`${method}: status ${r.res.status} ${JSON.stringify(r.res.error)}`);
  }
}

let connected = false;

export default function () {
  if (!connected) {
    try { connect(); } catch (_) { /* resilient() reconnects */ }
    connected = true;
  }
  const amount = 1 + Math.floor(Math.random() * 100);
  if (Math.random() < TOPUP_SHARE) {
    op('TopUp', { wallet_id: pick(payers), amount });
    return;
  }
  const src = pick(payers);
  let dst = merchant;
  if (Math.random() >= HOT_DEST_SHARE) {
    while (dst === merchant || dst === src) dst = pick(payers);
  }
  op('CreateTransfer', { source_id: src, dest_id: dst, amount });
}
