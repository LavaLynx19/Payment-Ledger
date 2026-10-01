// Feature mix: P2P, PlaceHold + partial Capture or Release, Reversals,
// Repayments, TopUps and Withdrawals, all running concurrently. Business
// rejections (insufficient funds, open Receivable, not reversible, nothing to
// repay) are expected and counted separately. Anything else counts as a
// failure, and the threshold makes k6 exit non-zero. The checker then has to
// come back clean.
import grpc from 'k6/net/grpc';
import { Counter } from 'k6/metrics';
import { code, callRetrying, connect, fund, pick, wallets } from './lib.js';

const FUNDING = Number(__ENV.FUNDING || 10000);

const ok = new Counter('ops_ok');
const rejected = new Counter('ops_rejected_expected');
const failed = new Counter('ops_failed');

export const options = {
  scenarios: {
    mix: { executor: 'constant-vus', vus: Number(__ENV.VUS || 30), duration: __ENV.DURATION || '30s' },
  },
  thresholds: { ops_failed: ['count==0'] },
  setupTimeout: '120s',
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  fund(wallets, FUNDING);
}

// Codes each operation may legitimately return besides OK.
const EXPECTED = {
  CreateTransfer: [grpc.StatusFailedPrecondition],
  PlaceHold: [grpc.StatusFailedPrecondition],
  CaptureHold: [grpc.StatusFailedPrecondition],
  ReleaseHold: [grpc.StatusFailedPrecondition],
  ReverseTransfer: [grpc.StatusFailedPrecondition],
  Repay: [grpc.StatusFailedPrecondition, grpc.StatusInvalidArgument],
  TopUp: [],
  Withdraw: [grpc.StatusFailedPrecondition],
};

function op(method, body) {
  const res = callRetrying(method, body);
  if (code(res.status) === code(grpc.StatusOK)) {
    ok.add(1, { op: method });
  } else if (EXPECTED[method].map(code).includes(code(res.status))) {
    rejected.add(1, { op: method });
  } else {
    failed.add(1, { op: method });
    console.error(`${method}: status ${res.status} ${JSON.stringify(res.error)}`);
  }
  return res;
}

const pair = () => {
  const src = pick(wallets);
  let dst = src;
  while (dst === src) dst = pick(wallets);
  return [src, dst];
};
const amount = (max) => 1 + Math.floor(Math.random() * max);

// Transfers this VU created, reversed later once they have had time to post.
const mine = [];
let connected = false;

export default function () {
  if (!connected) {
    connect();
    connected = true;
  }
  const roll = Math.random();
  const [src, dst] = pair();

  if (roll < 0.35) {
    const res = op('CreateTransfer', { source_id: src, dest_id: dst, amount: amount(200) });
    if (code(res.status) === code(grpc.StatusOK)) mine.push({ id: res.message.transfer.id, at: Date.now() });
  } else if (roll < 0.55) {
    const res = op('PlaceHold', { source_id: src, dest_id: dst, amount: amount(200) });
    if (code(res.status) !== code(grpc.StatusOK)) return;
    const holdId = res.message.hold.id;
    if (Math.random() < 0.7) {
      const capture = Math.random() < 0.5 ? undefined : amount(Number(res.message.hold.amount));
      const cap = op('CaptureHold', capture === undefined ? { hold_id: holdId } : { hold_id: holdId, amount: capture });
      if (code(cap.status) === code(grpc.StatusOK)) mine.push({ id: res.message.transfer.id, at: Date.now() });
    } else {
      op('ReleaseHold', { hold_id: holdId });
    }
  } else if (roll < 0.65) {
    const i = mine.findIndex((t) => Date.now() - t.at > 1000);
    if (i >= 0) op('ReverseTransfer', { transfer_id: mine.splice(i, 1)[0].id });
  } else if (roll < 0.8) {
    op('Repay', { wallet_id: src, amount: amount(100) });
  } else if (roll < 0.9) {
    op('TopUp', { wallet_id: src, amount: amount(500) });
  } else {
    op('Withdraw', { wallet_id: src, amount: amount(200) });
  }
}
