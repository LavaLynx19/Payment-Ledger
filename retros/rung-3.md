# Rung 3 retro: hot Account contention

Status: **done**. Stage 1 (lock-based capture, no CAS on System sources) and stage 2 (batched capture) meet the target. Stage 3 (sub-accounts) wasn't needed.

Environment: as before (M4 Pro, Docker Compose, relative numbers), measured on a cool machine.

## Starting point (P4.0)
| Run | Throughput | p99 |
|---|---|---|
| Rung 1 baseline (uniform P2P, 100 Wallets) | 2,000 posted/s | 6.27 ms |
| Rung 2 fault-free mix | 1,981 ops/s | 9.84 ms |

CAS counters (`expvar` map `cas`, A§6) for the Rung 2 mix, with no hot Account:

| Op | Attempts | Conflicts | Rate |
|---|---|---|---|
| accept | 76,795 | 4,583 | 6.0% |
| capture | 81,170 | 5,594 | 6.9% |
| reverse | 8,047 | 577 | 7.2% |
| release | 4,259 | 281 | 6.6% |

Even uniform traffic conflicts. Accept bumps the source version, and capture CASes both source and destination, so the two paths race on every Wallet.

## Hot Accounts (P4.1, `harness/run.sh 3`, 2,000/s for 30s, 100 Wallets, cool-downs between runs)
| Run | ops/s | Posted/s end to end | p99 | accept conflicts / exhausted | capture conflicts / exhausted | Drain |
|---|---|---|---|---|---|---|
| Hot destination (`HOT_DEST_SHARE=0.8`, no TopUps) | 1,869 | **1,403** | 5.12 ms | 1,195 / 61,295 (2%), 0 | **64,290 / 124,898 (51%), 2,979** | 11s |
| Hot funding (`TOPUP_SHARE=0.2`, uniform P2P) | 1,871 | 1,590 (P2P is 80%) | **17.0 ms** | **40,319 / 100,420 (40%), 1,346** | 25,474 / 87,119 (29%), 201 | 1s |
| Both | 1,873 | **1,256** | 9.54 ms | 14,610 / 74,711 (20%), 194 | 61,065 / 121,709 (50%), 1,721 | 7s |

All runs were CLEAN, with 0 failed and 0 given-up client operations. Every ABORTED was retried with the same key.

### Reading
- **Hot destination breaks capture, not accept.** Accept CASes only the payer, so it's barely affected. Every capture CASes the merchant row, so with optimistic CAS and immediate retry, half of all capture attempts conflict. The worker falls behind (posted/s 1,403 against 1,869 accepted, an 11s backlog). The client-facing p99 looks healthy because capture is asynchronous, which hides the problem.
- **Hot funding breaks accept latency.** Every TopUp's accept CASes the funding row (A§5 step 4 says to CAS every source), even though a System account has no funds check to protect. 40% of accepts conflict and 1,346 ran out of retries, so p99 hits 17 ms.
- **Both together** compound: the lowest posted/s (1,256).

## Stage 1 (P4.3): lock-based capture + no accept CAS on System sources
Same three runs, same settings, with cool-downs between them.

| Run | Posted/s (P4.1 → stage 1) | p99 | Capture conflicts | Accept conflicts | Drain |
|---|---|---|---|---|---|
| Hot destination | 1,403 → **1,941** | 5.12 → 8.27 ms | 51% (2,979 exhausted) → **0 / 60,902** | 2% → 3% | 11s → **0s** |
| Hot funding | 1,590 → **1,600** (all P2P) | 17.0 → **4.62 ms** | 29% → **0 / 63,980** | 40% (1,346 exhausted) → **3%** | 1s → **0s** |
| Both | 1,256 → **1,600** (all P2P) | 9.54 → **4.74 ms** | 50% → **0 / 62,888** | 20% → **2%** | 7s → **0s** |

Every run sustained the offered 2,000 ops/s (60,000 in 30s) with no capture backlog and a CLEAN checker.

- **Retry storms are gone.** Every capture is one attempt. Contention became queueing on the row lock, which at this rate is cheap.
- **The funding row is no longer a hot key for Accept.** The remaining 2–3% accept conflicts are ordinary payer-Wallet races, matching P4.0's uniform traffic.
- **p99:** two of three runs are well inside the 7.6 ms gate. The hot-destination run (8.27 ms) is over, yet the "both" run, whose contention is a superset, came in at 4.74 ms. That points to run noise. The hot-destination run went first, straight after the image build.

### Hot-destination p99 is real, not noise
- Rerun after a cool-down: p99 **7.98 ms** (the first run measured 8.27 ms), with 2,000 posted/s and 0 capture conflicts. That's reproducibly about 5% over the 7.6 ms gate.
- Diagnosis: the same run with `harness/diag/run-with-waits.sh` sampling `pg_stat_activity` every 0.2s (p99 9.89 ms in this run). 208 backend-samples were taken under load:

| Wait | Samples | Share |
|---|---|---|
| Row lock on capture's `UPDATE accounts` (`Lock:transactionid` 65 + `Lock:tuple` 14) | 79 | 38% |
| WAL flush at commit (`LWLock:WALWrite` 21 + `IO:WalSync` 20) | 41 | 20% |
| CPU / client round-trips, spread over statements | rest | ~42% |

Captures queue on the merchant row and hold its lock through commit, so the WAL flush sits inside the hold time and lengthens the queue. The flush is shared, so client-facing Accepts wait on it too. That's how a worker-side queue reaches client p99. Per the staged plan, a merchant-row lock wait means **stage 2 (batching)**, which also cuts commits.

## Stage 2 (P4.4): batched capture
The worker captures up to 100 of the oldest Holds per tx, greedily with no wait. It makes one netted row update per Account (version bumped by k) and one commit per batch. Same three runs with cool-downs, each sampled for waits.

| Run | p99 (P4.1 → stage 1 → stage 2) | Posted/s | Capture txs (stage 1 → 2) | Waits under load |
|---|---|---|---|---|
| Hot destination | 5.12 → 7.98–9.89 → **5.57 ms** | **2,000** | 60,902 → **21,724** | 208 → **16** samples; row lock 79 → **1**; rest WAL |
| Hot funding | 17.0 → 4.62 → **5.53 ms** | 1,604 (all P2P) | 63,980 → **22,742** | 34 samples, mostly `WALWrite` |
| Both | 9.54 → 4.74 → **4.83 ms** | 1,598 (all P2P) | 62,888 → **22,397** | 16 samples, mostly `WALWrite` |

All runs were CLEAN with 0 capture conflicts, accept conflicts at 3–4% (payer-Wallet races), and no backlog.

- Batches averaged about 2.8 Holds. Under load the netting is automatic, and at low load a batch is one Hold, so idle capture latency doesn't go up.
- The merchant row-lock queue is gone. What's left is WAL write at commit, at a much lower level: the floor for durable commits.

## Verdict
| Gate (README Rung 3) | Result |
|---|---|
| Correctness under hot keys | **Pass**: CLEAN in every run, 0 failed, 0 given-up |
| ≥ 2,000 ops/s with most traffic on one hot Account | **Pass**: the offered 2,000/s sustained with 0 backlog. Hot destination posts 2,000/s; the funding runs post all of their P2P |
| p99 ≤ 7.6 ms without faults | **Pass**: 4.83–5.57 ms in all three runs |

Stage 3 (sub-accounts) stays deferred: one batched row keeps up. Revisit if Rung 4's sharding concentrates a hot key further.

## Regression: Rung 2 fault matrix with batched capture
Stages 1-2 rewrote capture, which is where Rung 2's crash guarantees live, so the full matrix was rerun (`harness/faults/matrix.sh`, 2,000/s, 40s per run).

- **All 11 runs PASS.** 27 failpoint crashes plus 6 kill -9s (api, worker, Postgres): 0 lost, 0 not posted, 0 keys mapped to a different Transfer, CLEAN everywhere. A crash mid-capture now rolls back a whole batch, and recovery re-claims it with the money posted once.
- **Capture failpoints fired only once each** (4–7 in Rung 2). Batching cut capture txs to about a third, so fixed per-tx probabilities hit less often. Raise them about 3× for equal coverage next time.
- **Crash-time p99 spiked on two api-side runs**: accept.after_commit 3.12 s (7 crashes in 40s), reversal.after_commit 1.07 s. Clustered crashes escalate Docker's restart backoff, and clients wait out the restart. These are api-side failpoints that capture doesn't touch, so batching isn't the cause. Crash-time p99 isn't gated (Decision Log → *Rung gates*), and every request converged.

## Lessons
1. **A version check only belongs where it guards a read-dependent decision.** CAS everywhere turned a hot row into a retry storm (51% conflicts). Lock-based posting where funds were already reserved turned it into a short queue.
2. **Async paths hide contention from client latency.** The hot-destination run's p99 looked healthy at first, while the worker fell 11s behind. Posted/s end to end and the CAS counters exposed it.
3. **Diagnose before building.** Wait-event sampling (`run-with-waits.sh`) showed the hot row's lock queue (38%) and the WAL flush inside it (20%). Batching targets both, and its result could be checked against the same measurement.
4. **Batching works best when it never waits.** A greedy batch costs nothing at low load and nets automatically under load.
5. **Keep invariants intact while optimizing.** Bumping the version by k with consecutive Entry versions kept point-in-time reads and invariant 4 exact, so the checker still applies unchanged.

## Earlier evidence
- **Rung 1 setup:** TopUps from the single funding account got ABORTED (CAS retries exhausted) while captures posted to the same row.
- **Rung 2 setup:** funding 100 Wallets back to back failed even after 10s of client retries. Wallets are now funded one at a time.
