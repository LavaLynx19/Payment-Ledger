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
  });
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
    if (res.status !== grpc.StatusAborted) return res;
    sleep(0.005 * (i + 1));
  }
  return res;
}

// fund tops up each Wallet by amount and waits until the worker has posted
// it all, so the load phase starts from known balances. Call from setup().
export function fund(list, amount) {
  connect();
  for (const w of list) {
    // Every TopUp debits the one funding account, a hot key (Rung 3).
    const res = callRetrying('TopUp', { wallet_id: w, amount });
    if (res.status !== grpc.StatusOK) {
      throw new Error(`TopUp ${w}: ${JSON.stringify(res.error)}`);
    }
  }
  const deadline = Date.now() + 60_000;
  for (const w of list) {
    while (Number(call('GetBalance', { account_id: w }).message.posted) !== amount) {
      if (Date.now() > deadline) throw new Error(`wallet ${w} never reached ${amount}`);
      sleep(0.2);
    }
  }
  client.close();
}
