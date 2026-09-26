// Rung 1: many concurrent debits against a few funded senders, paying into
// receivers that never send. The naive Accept lets two Transfers pass the
// same funds check. Because money only flows one way, nothing refills an
// overspent sender, so the checker reports it both in the Entry history and in
// the final balance (invariant 3).
import grpc from 'k6/net/grpc';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';
import exec from 'k6/execution';

const TARGET = __ENV.TARGET || 'api:8080';
const TOKEN = __ENV.LEDGER_TOKEN || 'dev-token';
const FUNDING = Number(__ENV.FUNDING || 1000);
const MAX_AMOUNT = Number(__ENV.MAX_AMOUNT || 250);
const wallets = JSON.parse(open('/out/seed.json')).wallet_ids;
const SENDERS = Number(__ENV.SENDERS || Math.floor(wallets.length / 2));
const senders = wallets.slice(0, SENDERS);
const receivers = wallets.slice(SENDERS);

const client = new grpc.Client();
client.load(['/proto'], 'ledger/v1/ledger.proto');

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

function params() {
  return {
    metadata: {
      authorization: `Bearer ${TOKEN}`,
      'idempotency-key': `k6-${Date.now()}-${Math.random().toString(36).slice(2)}`,
    },
  };
}

function call(method, body) {
  return client.invoke(`ledger.v1.LedgerService/${method}`, body, params());
}

const pick = (list) => list[Math.floor(Math.random() * list.length)];

// setup funds every sender with FUNDING and waits until the worker has
// posted all of it, so the load phase starts from known balances.
export function setup() {
  if (senders.length === 0 || receivers.length === 0) {
    throw new Error(`need senders and receivers; got ${senders.length}/${receivers.length}`);
  }
  client.connect(TARGET, { plaintext: true });
  for (const w of senders) {
    const res = call('TopUp', { wallet_id: w, amount: FUNDING });
    if (res.status !== grpc.StatusOK) {
      throw new Error(`TopUp ${w}: ${JSON.stringify(res.error)}`);
    }
  }
  const deadline = Date.now() + 60_000;
  for (const w of senders) {
    while (Number(call('GetBalance', { account_id: w }).message.posted) !== FUNDING) {
      if (Date.now() > deadline) throw new Error(`sender ${w} never reached ${FUNDING}`);
      sleep(0.2);
    }
  }
  client.close();
}

export default function () {
  if (exec.vu.iterationInScenario === 0) {
    client.connect(TARGET, { plaintext: true });
  }
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
