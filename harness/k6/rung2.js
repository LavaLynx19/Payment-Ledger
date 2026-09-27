// Rung 2: steady load while processes crash (failpoints or kill -9). Every
// request retries with one Idempotency-Key until it gets a definite answer.
// Each acknowledged P2P Transfer is logged as "ACK <key> <id>", and run.sh
// then checks that every one exists and posted exactly once. The mix also
// drives the other crash points: Holds that expire (sweeper), place then
// release, and reversals.
import grpc from 'k6/net/grpc';
import { Counter } from 'k6/metrics';
import { code, connect, fund, pick, resilient, wallets } from './lib.js';

const FUNDING = Number(__ENV.FUNDING || 1_000_000_000);
const RATE = Number(__ENV.RATE || 2000);

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
  fund(wallets, FUNDING);
}

const EXPECTED = {
  CreateTransfer: [grpc.StatusFailedPrecondition],
  PlaceHold: [grpc.StatusFailedPrecondition],
  ReleaseHold: [grpc.StatusFailedPrecondition],
  ReverseTransfer: [grpc.StatusFailedPrecondition],
};

function op(method, body) {
  const r = resilient(method, body);
  if (r.gaveUp) {
    gaveUp.add(1, { op: method });
    console.error(`${method}: gave up with outcome unknown (key ${r.key})`);
  } else if (code(r.res.status) === code(grpc.StatusOK)) {
    ok.add(1, { op: method });
  } else if (EXPECTED[method].map(code).includes(code(r.res.status))) {
    rejected.add(1, { op: method });
  } else {
    failed.add(1, { op: method });
    console.error(`${method}: status ${r.res.status} ${JSON.stringify(r.res.error)}`);
  }
  return r;
}

const pair = () => {
  const src = pick(wallets);
  let dst = src;
  while (dst === src) dst = pick(wallets);
  return [src, dst];
};
const amount = () => 1 + Math.floor(Math.random() * 100);

const mine = []; // this VU's acknowledged P2P Transfers, for later reversal
let connected = false;

export default function () {
  if (!connected) {
    try { connect(); } catch (_) { /* resilient() reconnects */ }
    connected = true;
  }
  const roll = Math.random();
  const [src, dst] = pair();

  if (roll < 0.8) {
    const r = op('CreateTransfer', { source_id: src, dest_id: dst, amount: amount() });
    if (!r.gaveUp && code(r.res.status) === code(grpc.StatusOK)) {
      const id = r.res.message.transfer.id;
      console.log(`ACK ${r.key} ${id}`);
      mine.push({ id, at: Date.now() });
    }
  } else if (roll < 0.85) {
    op('PlaceHold', { source_id: src, dest_id: dst, amount: amount(), ttl: '1s' }); // left to expire
  } else if (roll < 0.9) {
    const r = op('PlaceHold', { source_id: src, dest_id: dst, amount: amount() });
    if (!r.gaveUp && code(r.res.status) === code(grpc.StatusOK)) op('ReleaseHold', { hold_id: r.res.message.hold.id });
  } else {
    const i = mine.findIndex((t) => Date.now() - t.at > 2000);
    if (i >= 0) op('ReverseTransfer', { transfer_id: mine.splice(i, 1)[0].id });
  }
}
