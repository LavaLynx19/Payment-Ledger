# Rung 4 retro: sharding, cross-shard Transfers, and TigerBeetle

Status: **done**. Sharded Postgres (2PC by default, saga as a variant) and TigerBeetle were measured against each other as ratios (README). Every run's checker was CLEAN, and the fault matrix lost or duplicated no money. Measurement found three 2PC bugs, each fixed with a regression test before the final runs.

Environment: as before (M4 Pro, Docker Compose, relative numbers). Both Postgres shards, TigerBeetle, the app and k6 share one machine. Each configuration ran once, with 60s cool-downs between runs.

## Throughput at the Rung 1 target (Batch A, 2,000/s for 30s, 100 Wallets)
| Run | Posted/s end to end | p99 | Checker |
|---|---|---|---|
| A1 one shard (reference) | 2,001 | 11.3 ms | CLEAN |
| A2 two shards, 2PC | 1,998 | 34.8 ms | CLEAN |
| A3 two shards, saga | 2,000 | 8.2 ms | CLEAN |
| A4 TigerBeetle | ≈1,983 (accept = post) | 26.9 ms | CLEAN |
| A5 hot Accounts, 2PC (`HOT_DEST_SHARE=0.8 TOPUP_SHARE=0.2`) | 1,604 | 10.7 ms | CLEAN |
| A6 hot Accounts, saga | 1,599 | 7.8 ms | CLEAN |
| A7 hot Accounts, TigerBeetle | ≈1,990 | 17.5 ms | CLEAN |

Every engine keeps up with a fixed 2,000/s, so these runs can't give the ratios the README asks for. Single-run p99 also varied 2–3× between repeats (A1 measured 4.3 ms and 11.3 ms an hour apart), so treat it as a rough indicator only. Hot-run posted/s tops out near 1,600 because 20% of operations are TopUps.

## Capacity (`harness/k6/saturate.js`, 2k→6k/s in 10s steps)
Capacity is the highest offered rate with under 1% dropped iterations.

| Configuration | Capacity | Max achieved | p99 at capacity | Ratio to one shard |
|---|---|---|---|---|
| One shard | 4,000/s | 4,720/s | 41 ms | 1.0× |
| Two shards, 2PC | 2,000/s | 2,589/s | 14 ms | **0.5×** |
| Two shards, saga | 3,000/s | 3,493/s | 18 ms | **0.75×** |
| TigerBeetle | ≥ 5,000/s (not saturated) | 5,934/s | 52 ms | **≥ 1.25×** |

### Reading
- **Sharding on one machine costs capacity.** The shards share CPU, so splitting them adds coordination without adding hardware. About half of all Accepts become 2PC because the key's shard differs from the source's (Decision Log → *Sharded idempotency keys join the 2PC*), and cross-shard captures add more.
- **2PC's ceiling is its own protocol cost, not the admission cap.** With 32 connections per pool (31 slots against 13), the maximum achieved rate rose only 12% (2,901/s), while accept conflicts rose from 31% to 41% and 1,567 Transfers exhausted their CAS retries.
- **The saga is cheaper than 2PC** because capture credits travel through the outbox instead of a two-shard commit. Accept still uses 2PC, which is why it doesn't reach one-shard capacity.
- **TigerBeetle has the highest capacity** and held p99 under 52 ms up to 6,000/s, while Postgres went to 0.5–1.5 s past its capacity. Two caveats remain open. Its p99 at low load is higher than Postgres's (17–27 ms against 8–11 ms), and I haven't diagnosed that. At 6k/s a single k6 container may be the limit rather than TigerBeetle.

## Fault matrix (Batch B, `harness/run.sh 2`, 40s, `AGE_KEYS=1`)
| Run | Faults | ops/s | p99 | Gave up | Checker + acknowledged |
|---|---|---|---|---|---|
| B1 2PC `twopc.after_first_prepare` | 2 api, 1 worker crash | 1,685 | 2.3 s | 0 | CLEAN |
| B2 2PC `twopc.after_all_prepared` | 3 api, 1 worker | 1,572 | 2.6 s | 0 | CLEAN |
| B3 2PC `twopc.after_decision` | 3 api | 1,584 | 2.8 s | 0 | CLEAN |
| B4 2PC `twopc.after_first_commit` | 1 api | 1,791 | 1.8 s | 0 | CLEAN |
| B5 saga `saga.after_credit` | 8 worker | 1,902 | 102 ms | 0 | CLEAN |
| B6 2PC kill -9 (api, worker, both Postgres) | 7 kills | 1,477 | 2.9 s | 0 | CLEAN |
| B7 saga kill -9 | 7 kills | 1,547 | 1.9 s | 0 | CLEAN |
| B8 TigerBeetle kill -9 (api, tigerbeetle) | 4 kills | 1,810 | 1.0 s | 0 | CLEAN |
| B9 2PC partition (`shard1:12:3`, toxiproxy) | 3 partitions | 1,000 | 2.0 s | 0 | CLEAN |
| B10 saga partition | 3 partitions | 1,170 | 1.4 s | 0 | CLEAN |

"Acknowledged" means `checker --acks`: every Transfer a client was told succeeded exists, is posted, and its key still maps to it. The resolver finished every crashed 2PC, and B5's relay found 2 credits already applied after crashes, which it skipped rather than applying twice. Crash-time p99 is recorded, not gated (Decision Log → *Rung gates*).

## What broke, why, and what changed
1. **The two shards' connection pools deadlocked each other.** The first two-shard run collapsed from 2,000 to 121 ops/s, with p99 pinned at the 5s request deadline. Postgres logged no lock timeouts, only cancellations. An XTx holds one shard's connection while it opens the next, so under load each pool filled with transfers waiting on the other. Pool size alone moved the failure point: 4 connections collapsed at once, 64 ran clean. **Fix:** a per-process admission cap of `pool_max_conns − 1` XTx when there's more than one shard (A§9.2, Decision Log → *2PC admission cap instead of ordered acquisition*). Regression test: `TestXTxPoolsCannotDeadlock`.
2. **Client deadlines stranded prepared txs.** The commit phase ran on the request's context, so a deadline that fired after PREPARE also cancelled the cleanup. **Fix:** from the first PREPARE on, the commit runs on a detached context with its own bounded timeout (A§9.4). The harness drain also waits for `pg_prepared_xacts` to empty before stopping the worker.
3. **A crashed coordinator's locks outlived the client's patience.** With the first two fixes in, the fault runs kept every invariant, but 32–238 client operations per run gave up. Prepared txs held their row locks until the resolver's 10s `PREPARE_TIMEOUT`, and transfers queued behind them hit the 5s `lock_timeout` twice. **Fix:** `PREPARE_TIMEOUT` now defaults to 2s. In the same run give-ups fell from 238 to 1 and ops/s rose from 800 to 1,532 (Decision Log → *2PC prepare timeout: 2s, not 10s*).
4. **Cross-shard replays failed.** The last 1–2 give-ups per run were all ReleaseHold or ReverseTransfer, never CreateTransfer, and each one's last status was Internal. A retry that found its key already claimed returned before naming the 2PC's home Transfer, so its two-shard tx failed with "no home Transfer" on every attempt. CaptureHold had the same bug. On one shard the commit is local, which is why Rung 2 never saw it. **Fix:** the home is set before the key is claimed. Regression test: `TestCrossShardReplays`.

## Harness bugs found along the way
- `harness/run.sh` called `docker compose run` inside the batch driver's `while read` loop, and it consumed the rest of the run list from stdin. Only A1 ran. Each run now gets `< /dev/null`.
- The fault scripts built their own compose command without the shards profile or the TigerBeetle overlay. They would have failed to kill `postgres-shard1` or `tigerbeetle`, and would have restarted the TigerBeetle-run api on the wrong image. `run.sh` now exports `COMPOSE_FILE`/`COMPOSE_PROFILES` for them.
- The TigerBeetle audit looked up 8,189 acknowledged ids per request, which the client rejected as too much data. It now uses batches of 1,024.
- `saga.after_credit` fires once per relay batch, not once per Transfer. At 0.00008 it never fired in two runs; it needed 0.0015.
- The give-up log line now records the last status. That is what found bug 4.

## Not covered
- `AGE_KEYS` on two shards ages only keys whose Transfer lives on the same shard, about half of them. The purge path is only partly exercised there.
- TigerBeetle runs single-replica in development mode. Its kill -9 recovery is one replica restarting from its data file, not a failover.
- TigerBeetle's p99 at low load and its true capacity ceiling are both undiagnosed (see *Reading*).
- Each configuration ran once. The ratios are robust at this size (0.5×, 0.75×, ≥ 1.25×), but individual p99 values are not.

## Verdict
1. **Load (relative target):** reported as ratios. Two-shard 2PC 0.5×, saga 0.75×, TigerBeetle ≥ 1.25× the one-shard capacity.
2. **Invariants:** every run CLEAN on both engines, with the global cross-shard checks (A§9.6) and the TigerBeetle audit (A§9.7).
3. **Faults:** 4 2PC crash points, the saga crash point, kill -9 on every process and store, and network partitions. No money lost or duplicated, every acknowledged Transfer posted once, and 0 give-ups on the final code.
4. **Retro:** this document.

## Lessons
1. **Connection pools are locks too.** A distributed tx that holds one resource while acquiring another can deadlock on pools as easily as on rows, and no database will see the cycle. Bound concurrency below the smallest pool, or acquire in a fixed order.
2. **Measure availability, not just safety.** The checker was CLEAN through bugs 2–4. Only the client-side give-up count exposed them. Recovery that's correct but takes 10s is still an outage for the client.
3. **Retries take paths that first attempts don't.** All of bug 4 lived in the replay branch, which only a crash followed by a same-key retry can reach. Fault injection plus idempotent retries is what covers it.
4. **Sharding on one box measures coordination cost, not scale-out.** The useful number here is the overhead per protocol (2PC 0.5×, saga 0.75×). A scale-out claim needs shards on separate hardware, which ties into the deployment-scale experiment deferred in README.
5. **A purpose-built engine moves the problem.** TigerBeetle needed no 2PC, resolver or admission cap, and was the fastest here. The cost is TigerBeetle's own data model, with linked chains instead of SQL invariants (A§9.7).
