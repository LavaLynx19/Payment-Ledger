// Rung 1: many concurrent debits against a few funded senders, paying into
// receivers that never send. The naive Accept lets two Transfers pass the
// same funds check. Because money only flows one way, nothing refills an
// overspent sender, so the checker reports it both in the Entry history and in
// the final balance (invariant 3).
import grpc from 'k6/net/grpc';
import { Counter } from 'k6/metrics';
import exec from 'k6/execution';
import { call, connect, fund, pick, wallets } from './lib.js';

const FUNDING = Number(__ENV.FUNDING || 1000);
const MAX_AMOUNT = Number(__ENV.MAX_AMOUNT || 250);
const SENDERS = Number(__ENV.SENDERS || Math.floor(wallets.length / 2));
const senders = wallets.slice(0, SENDERS);
const receivers = wallets.slice(SENDERS);

const accepted = new Counter('transfers_accepted');
const rejected = new Counter('transfers_rejected_insufficient');
const failed = new Counter('transfers_failed');

export const options = {
  scenarios: {
    transfers: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 50),
      duration: __ENV.DURATION || '30s',
    },
  },
  setupTimeout: '120s',
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  if (senders.length === 0 || receivers.length === 0) {
    throw new Error(`need senders and receivers; got ${senders.length}/${receivers.length}`);
  }
  fund(senders, FUNDING);
}

export default function () {
  if (exec.vu.iterationInScenario === 0) connect();
  const amount = 1 + Math.floor(Math.random() * MAX_AMOUNT);
  const res = call('CreateTransfer', { source_id: pick(senders), dest_id: pick(receivers), amount });
  if (res.status === grpc.StatusOK) {
    accepted.add(1);
  } else if (res.status === grpc.StatusFailedPrecondition) {
    rejected.add(1);
  } else {
    failed.add(1);
    console.error(`CreateTransfer: status ${res.status} ${JSON.stringify(res.error)}`);
  }
}
