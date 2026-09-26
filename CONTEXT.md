# payment-ledger

Vocabulary for a double-entry balance ledger with wallet and P2P flows, built up across progressively harder Rungs.

## Accounts

**Account**:
Anything in the ledger that holds a balance made up of Entries. Every Account is either a Wallet or a System account.
_Avoid_: Ledger account, balance holder

**Wallet**:
A user-owned Account whose Available balance can never go below zero.
_Avoid_: User account, customer account, purse

**System account**:
A platform-owned Account, such as funding or receivables, that is not bound by the never-negative rule.
_Avoid_: Internal account, house account, platform wallet

**Linked bank account**:
A user's simulated external bank account. It is the counterparty for Top-ups and Withdrawals and is not an Account in the ledger.
_Avoid_: External account, bank wallet, funding source

## Ledger

**Entry**:
A single immutable debit or credit of an amount against one Account. Entries are never edited or deleted.
_Avoid_: Line item, posting, row

**Idempotency key**:
A caller-chosen identifier, bound to one request payload, that ensures a money movement happens at most once no matter how many times it is submitted.
_Avoid_: Request ID, dedupe key

## Money movement

**Transfer**:
A movement of an amount from one Account to another, recorded as balanced Entries. Every Transfer starts as a Hold and stays PENDING until it is Captured. If its Hold expires first, the Transfer fails.
_Avoid_: Payment, transaction, P2P

**Hold**:
A reservation of funds on a source Account for a destination fixed when the Hold is placed. A Hold ends when it is Captured, Released, or expires. Holds placed for a Reversal never expire.
_Avoid_: Authorization, reservation, pending debit

**Capture**:
Turning all or part of a Hold into posted Entries paid to the Hold's destination. A Hold can be Captured only once, and any uncaptured remainder is Released.
_Avoid_: Settle, post, complete

**Release**:
Ending a Hold without moving money, so the reserved amount returns to Available balance.
_Avoid_: Void, cancel

**Reversal**:
A new Transfer that sends money back for a mistaken Transfer. The original Transfer is never changed.
_Avoid_: Refund, void, correction, chargeback

**Top-up**:
A Transfer from a System account into a Wallet that stands in for money arriving from a Linked bank account.
_Avoid_: Deposit, load, add funds

**Withdrawal**:
A Transfer from a Wallet into a System account that stands in for money leaving to a Linked bank account.
_Avoid_: Cash-out, payout

**Repayment transfer**:
A Transfer that pays down an open Receivable. It is the only debit allowed while the debtor has one.
_Avoid_: Settlement, payback

## Balances

**Posted balance**:
The sum of an Account's Entries.
_Avoid_: Ledger balance, actual balance

**Available balance**:
Posted balance minus the Account's active Holds. This is the amount that can currently be debited.
_Avoid_: Spendable balance, current balance

**Receivable**:
The amount a Wallet's owner still owes after a Reversal could not recover the full sum. It is recorded as a ledger balance.
_Avoid_: Debt, overdraft, negative balance, clawback

## Project

**Rung**:
One stage of the build ladder. It is defined by the specific correctness or scale problem it exposes and a TPS target relative to the Rung 1 baseline.
_Avoid_: Level, phase, milestone
