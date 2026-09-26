# payment-ledger

A double-entry balance ledger with wallet and P2P flows. It is built up one **Rung** at a time, and each Rung exposes a specific correctness or scale problem. This is a learning and interview-prep project. No real money moves.

Domain terms (**bold**) are defined in [CONTEXT.md](./CONTEXT.md).

## Problem Statement

Moving money between accounts sounds simple, but it breaks under concurrency, crashes, contention, and partitioning. Common failures are double-spends, lost updates, duplicated retries, and balances that don't match their history. This project builds a ledger that holds its invariants while load and failure modes get worse, and it documents why each fix is needed.

## Goals

- Record every money movement as balanced **Entries**, so that total debits always equal total credits.
- Support P2P **Transfers**, **Holds** (with **Capture** and **Release**), and **Reversals** between **Accounts**.
- Keep every invariant intact across all four **Rungs**.
- Serve three consumers: calling services, end-user UIs, and batch reporting.
- Produce a writeup that can be defended in an interview, with each decision traced to the Decision Log in ARCHITECTURE.md.

## Non-Goals

- KYC, spending limits, fraud or risk scoring
- Notifications (email, push)
- Real external money movement. **Top-ups** and **Withdrawals** are simulated through a **System account**, and the **Linked bank account** never touches a real bank.
- End-user identity and login (see Constraints → Auth)
- Multiple currencies or FX
- Absolute throughput claims. Rungs prove correctness, and TPS numbers are relative.

## User Stories

### Calling service (API)
- As a service, I submit a **Transfer** with an **Idempotency key**. The response gives me a transfer ID in PENDING state, and I read the final status later.
- As a service, when I retry after a timeout with the same **Idempotency key** and payload, I get the same transfer ID back and the money moves only once.
- As a service, when I send the same **Idempotency key** with a different payload, the request is rejected.
- As a service, when the source **Wallet** has insufficient **Available balance**, the Transfer is rejected.
- As a service, I place a **Hold** on a Wallet for a fixed destination. Later I either **Capture** it once, fully or partially, with any remainder Released automatically, or I **Release** it.
- As a service, I issue a **Top-up** or **Withdrawal** against a user's **Linked bank account** (simulated).
- As a service, I issue a **Reversal** for a mistaken Transfer. If the recipient has already spent part of the funds, the unrecoverable remainder becomes a **Receivable**.

### End user (UI)
- As a user, my **Available balance** already reflects a Transfer I just sent, even while it is PENDING.
- As a user, I can see my **Posted balance**, my **Available balance**, and my pending Holds.
- As a user, I can page through my transaction history.
- As a user with an open **Receivable**, I cannot send Transfers, but I can make a **Repayment transfer**.

### Reporting / ops
- As ops, I can get any Account's balance as of a past point in time.
- As ops, I can pull a statement of an Account's Entries over a date range.
- As ops, I can run a global audit that confirms the ledger is balanced (all Entries sum to zero), no Wallet is negative, and every balance matches its Entries.
- As ops, I can list open Receivables with debtor and age.

## Constraints

### Invariants
- A **Wallet** never goes negative. A Transfer or Capture that would make it negative is rejected. The only exception is a **Reversal**, which reverses what is available and records the rest as a **Receivable**, so the balance still stays non-negative.
- The ledger is append-only. Corrections are made only with **Reversals** and are never edits.
- Single currency, with amounts stored as integer minor units.
- An open **Receivable** blocks all outgoing debits from the debtor except **Repayment transfers**.

### Consistency
- Balance reads are strictly read-your-writes. A caller never sees a balance older than its own accepted writes.
- Accepting a **Transfer** places its **Hold** before PENDING is returned, so **Available balance** drops at accept time.
- If a Hold expires at the same moment a Capture arrives, whichever is committed first wins. Nothing can be both captured and expired. A PENDING Transfer whose Hold expires fails, and its funds return to Available balance. Holds the system places for a **Reversal** never expire, so a correction can't silently fail.
- A retry that arrives while the original request is still in flight gets the same transfer ID back in PENDING.

### Auth
- Callers authenticate as services with service tokens. User and Account IDs supplied by the caller are trusted.

### Environment
- Runs entirely on one local machine (MacBook M4 Pro). Multi-node and sharded Rungs are simulated on that machine, and network faults are injected synthetically.

## Rung Ladder

Each **Rung** has a named problem to expose and a TPS target. Targets are multiples of the baseline measured at Rung 1.

| # | Problem exposed | TPS target |
|---|-----------------|------------|
| 1 | Lost update / double-spend under concurrent debits | Baseline (measured) |
| 2 | Crash mid-Transfer: recovery with no lost or duplicated money | Set after Rung 1 |
| 3 | Hot Account contention (one Account in most Transfers) | Set after Rung 1 |
| 4 | Sharding and cross-shard Transfers | Set after Rung 1 |

Rung 4 is where the trade-off between strict reads, never-negative Wallets, and cross-shard Transfers gets decided. Designs for earlier Rungs must not rule out any of those choices.

## Success Criteria

### Per Rung
A Rung is done when all four checks pass:
1. **Load:** the load test sustains the TPS target at the stated p99 latency.
2. **Invariants:** the independent invariant checker finds zero violations.
3. **Faults:** after killing a process or storage mid-Transfer and recovering, no money is lost or duplicated.
4. **Retro:** a written retro covers what broke, why, and what changed.

### Project
- All 4 Rungs pass.
- The design can be whiteboarded and every choice defended from the Decision Log.
- One command runs load, fault injection, and invariant checks for any Rung.
