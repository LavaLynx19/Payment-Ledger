// Baseline throughput: P2P Transfers among amply funded Wallets at a fixed
// arrival RATE, so nearly every request is accepted and the work measured is
// Accept + Capture end to end. Step RATE across runs. The sustained baseline
// is the highest RATE with no failures, no dropped iterations, and a prompt
// capture drain after load (see run.sh's posted/s report).
import grpc from 'k6/net/grpc';
import { Counter } from 'k6/metrics';
import { call, connect, fund, pick, wallets } from './lib.js';

const FUNDING = Number(__ENV.FUNDING || 1_000_000_000);
const RATE = Number(__ENV.RATE || 1000);

const accepted = new Counter('transfers_accepted');
const rejected = new Counter('transfers_rejected_insufficient');
const failed = new Counter('transfers_failed');

export const options = {
  scenarios: {
    transfers: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: Number(__ENV.VUS || 100),
      maxVUs: Number(__ENV.MAX_VUS || 400),
    },
  },
  setupTimeout: '120s',
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  fund(wallets, FUNDING);
}

// Each VU connects on first use; the arrival-rate executor may start VUs late.
let connected = false;

export default function () {
  if (!connected) {
    connect();
    connected = true;
  }
  const src = pick(wallets);
  let dst = src;
  while (dst === src) dst = pick(wallets);

  const res = call('CreateTransfer', { source_id: src, dest_id: dst, amount: 1 + Math.floor(Math.random() * 100) });
  if (res.status === grpc.StatusOK) {
    accepted.add(1);
  } else if (res.status === grpc.StatusFailedPrecondition) {
    rejected.add(1);
  } else {
    failed.add(1);
    console.error(`CreateTransfer: status ${res.status} ${JSON.stringify(res.error)}`);
  }
}
