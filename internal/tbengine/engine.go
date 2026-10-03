//go:build tigerbeetle

// Package tbengine implements the ledger API on a single TigerBeetle cluster
// (A§9.7). It returns the ledger package's domain errors and store's types, so
// internal/api serves it unchanged. It builds only with -tags tigerbeetle: the
// client links on Linux only.
//
// Posting is single-phase where possible: CreateTransfer, TopUp, Withdraw,
// Repay and Reversal post as one atomic transfer with limits enforced in the
// engine. Holds use pending / post_pending / void_pending.
package tbengine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"payment-ledger/internal/ledger"
	"payment-ledger/internal/shard"
	"payment-ledger/internal/store"
)

const ledgerID = 1

// Account codes.
const (
	codeWallet     uint16 = 1
	codeFunding    uint16 = 2
	codeReceivable uint16 = 3
	codeControl    uint16 = 4
)

// codeGuard marks control transfers: the receivable guard on Wallet debits.
const codeGuard uint16 = 9

// Transfer codes, one per transfer type.
var typeCodes = map[string]uint16{
	ledger.TypeP2P: 1, ledger.TypeTopUp: 2, ledger.TypeWithdrawal: 3,
	ledger.TypeReversal: 4, ledger.TypeReceivable: 5, ledger.TypeRepayment: 6,
}

func typeOf(code uint16) string {
	for t, c := range typeCodes {
		if c == code {
			return t
		}
	}
	return "control"
}

type Engine struct {
	c       tb.Client
	holdTTL time.Duration
	funding uuid.UUID
	guard   uuid.UUID // control D: zero balance, never debited; the receivable guard's source
	revC    uuid.UUID // control C: nets to zero; routes reversal chains
}

// New connects to TigerBeetle and makes sure the funding account exists.
func New(address string, holdTTL time.Duration) (*Engine, error) {
	addr, err := resolve(address)
	if err != nil {
		return nil, err
	}
	c, err := tb.NewClient(tb.ToUint128(0), []string{addr})
	if err != nil {
		return nil, fmt.Errorf("tigerbeetle client: %w", err)
	}
	e := &Engine{c: c, holdTTL: holdTTL, funding: derive("funding"),
		guard: derive("control-guard"), revC: derive("control-reversal")}
	if err := e.createAccounts(
		account(e.funding, codeFunding, tb.AccountFlags{History: true}),
		account(e.guard, codeControl, tb.AccountFlags{DebitsMustNotExceedCredits: true}),
		account(e.revC, codeControl, tb.AccountFlags{}),
	); err != nil {
		c.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) Close() { e.c.Close() }

// resolve turns host:port into ip:port. The client accepts only an IP (or a
// bare port), not a hostname, and Compose addresses services by name.
func resolve(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return address, nil // a bare port, or already an IP
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("resolve TigerBeetle host %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return net.JoinHostPort(ip.String(), port), nil
		}
	}
	return "", fmt.Errorf("resolve TigerBeetle host %q: no IPv4 address", host)
}

// FundingID is the funding System account (one: TigerBeetle isn't sharded).
func (e *Engine) FundingID() uuid.UUID { return e.funding }

// CreateWallets creates n Wallets that can never go negative, each with its
// receivable account (so the receivable guard always has a target), all with
// balance history for point-in-time reads.
func (e *Engine) CreateWallets(n int) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, n)
	accounts := make([]tb.Account, 0, 2*n)
	for i := range ids {
		id, err := shard.NewID(0)
		if err != nil {
			return nil, err
		}
		ids[i] = id
		recv := account(receivableID(id), codeReceivable, tb.AccountFlags{CreditsMustNotExceedDebits: true, History: true})
		recv.UserData128 = toTB(id) // the debtor
		accounts = append(accounts,
			account(id, codeWallet, tb.AccountFlags{DebitsMustNotExceedCredits: true, History: true}), recv)
	}
	return ids, e.createAccounts(accounts...)
}

// ---- ids ----

func toTB(id uuid.UUID) tb.Uint128   { return tb.BytesToUint128(id) }
func fromTB(id tb.Uint128) uuid.UUID { return uuid.UUID(id.Bytes()) }

// derive is a deterministic id: a v8 UUID from the hash of parts. Transfers
// keyed by an Idempotency-Key, and a Hold's capture/void, use it so the same
// request always maps to the same TigerBeetle id.
func derive(parts ...string) uuid.UUID {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0F | 0x80 // version 8
	id[8] = id[8]&0x3F | 0x80 // RFC 9562 variant
	return id
}

func keyID(key string) uuid.UUID         { return derive("key", key) }
func postID(hold uuid.UUID) uuid.UUID    { return derive("post", hold.String()) }
func voidID(hold uuid.UUID) uuid.UUID    { return derive("void", hold.String()) }
func receivableID(w uuid.UUID) uuid.UUID { return derive("receivable", w.String()) }
func nanos(t time.Time) uint64           { return uint64(t.UnixNano()) }
func at(ts uint64) time.Time             { return time.Unix(0, int64(ts)).UTC() }
func amountOf(x tb.Uint128) int64        { lo, _ := x.Uint64(); return int64(lo) }
func u128(n int64) tb.Uint128            { return tb.ToUint128(uint64(n)) }

func account(id uuid.UUID, code uint16, flags tb.AccountFlags) tb.Account {
	return tb.Account{ID: toTB(id), Ledger: ledgerID, Code: code, Flags: flags.ToUint16()}
}

func (e *Engine) createAccounts(accounts ...tb.Account) error {
	res, err := e.c.CreateAccounts(accounts)
	if err != nil {
		return fmt.Errorf("create accounts: %w", err)
	}
	for _, r := range res {
		if r.Status != tb.AccountCreated && r.Status != tb.AccountExists {
			return fmt.Errorf("create account: %s", r.Status)
		}
	}
	return nil
}

// submit sends transfers as one batch (linked ones succeed or fail together)
// and returns each one's status.
func (e *Engine) submit(ts ...tb.Transfer) ([]tb.CreateTransferStatus, error) {
	res, err := e.c.CreateTransfers(ts)
	if err != nil {
		return nil, fmt.Errorf("create transfers: %w", err)
	}
	out := make([]tb.CreateTransferStatus, len(ts))
	for i := range out {
		out[i] = tb.TransferCreated
	}
	for i, r := range res {
		if i < len(out) {
			out[i] = r.Status
		}
	}
	return out, nil
}

func existsDifferently(s tb.CreateTransferStatus) bool {
	return strings.HasPrefix(s.String(), "TransferExistsWithDifferent")
}

// ---- accounts ----

func (e *Engine) lookupAccounts(ids ...uuid.UUID) (map[uuid.UUID]tb.Account, error) {
	tbIDs := make([]tb.Uint128, len(ids))
	for i, id := range ids {
		tbIDs[i] = toTB(id)
	}
	res, err := e.c.LookupAccounts(tbIDs)
	if err != nil {
		return nil, fmt.Errorf("lookup accounts: %w", err)
	}
	m := make(map[uuid.UUID]tb.Account, len(res))
	for _, a := range res {
		m[fromTB(a.ID)] = a
	}
	return m, nil
}

// posted is the account's balance on its normal side: credit-normal for
// Wallets, debit-normal for funding and receivable accounts (A§4).
func posted(a tb.Account) int64 {
	dr, cr := amountOf(a.DebitsPosted), amountOf(a.CreditsPosted)
	if a.Code == codeWallet {
		return cr - dr
	}
	return dr - cr
}

func available(a tb.Account) int64 {
	if a.Code == codeWallet {
		return posted(a) - amountOf(a.DebitsPending)
	}
	return posted(a)
}

func kindOf(a tb.Account) string {
	if a.Code == codeWallet {
		return "wallet"
	}
	return "system"
}

// ---- writes ----

// endpoints mirrors the Postgres ledger's per-type Account kinds.
var endpoints = map[string]struct{ source, dest, field string }{
	ledger.TypeP2P:        {"wallet", "wallet", ""},
	ledger.TypeTopUp:      {"system", "wallet", "wallet_id"},
	ledger.TypeWithdrawal: {"wallet", "system", "wallet_id"},
	ledger.TypeRepayment:  {"wallet", "system", "wallet_id"},
}

func invalid(msg string) error { return &ledger.InvalidError{Msg: msg} }

// prepare validates a request and returns its source and destination accounts.
func (e *Engine) prepare(typ string, src, dst uuid.UUID, amount int64) (tb.Account, tb.Account, error) {
	if amount <= 0 {
		return tb.Account{}, tb.Account{}, invalid("amount must be greater than 0.")
	}
	if src == dst {
		return tb.Account{}, tb.Account{}, invalid("source_id and dest_id must be different accounts.")
	}
	accts, err := e.lookupAccounts(src, dst)
	if err != nil {
		return tb.Account{}, tb.Account{}, err
	}
	s, ok := accts[src]
	if !ok {
		return tb.Account{}, tb.Account{}, &ledger.NotFoundError{Resource: "account", ID: src.String()}
	}
	d, ok := accts[dst]
	if !ok {
		return tb.Account{}, tb.Account{}, &ledger.NotFoundError{Resource: "account", ID: dst.String()}
	}
	ep := endpoints[typ]
	switch {
	case kindOf(s) != ep.source && ep.field != "":
		return s, d, invalid(ep.field + " must be a wallet.")
	case kindOf(s) != ep.source:
		return s, d, invalid("source_id must be a " + ep.source + ".")
	case kindOf(d) != ep.dest && ep.field != "":
		return s, d, invalid(ep.field + " must be a wallet.")
	case kindOf(d) != ep.dest:
		return s, d, invalid("dest_id must be a " + ep.dest + ".")
	}
	return s, d, nil
}

// outcome maps a single transfer's status to the ledger's errors. A known id
// with identical fields is a replay; with different fields, a mismatch.
func (e *Engine) outcome(status tb.CreateTransferStatus, src uuid.UUID, amount int64) error {
	switch {
	case status == tb.TransferCreated, status == tb.TransferExists:
		return nil
	case existsDifferently(status):
		return ledger.ErrIdempotencyMismatch
	case status == tb.TransferIDAlreadyFailed:
		// TigerBeetle remembers ids that failed transiently (e.g. insufficient
		// funds), so a retry with the same key can't succeed later (A§9.7).
		return invalid("This Idempotency-Key was used by a request that failed. Use a new key.")
	case status == tb.TransferExceedsCredits:
		a, err := e.lookupAccounts(src)
		if err != nil {
			return err
		}
		return &ledger.InsufficientFundsError{Available: available(a[src]), Amount: amount}
	case status == tb.TransferDebitAccountNotFound:
		return &ledger.NotFoundError{Resource: "account", ID: src.String()}
	}
	return fmt.Errorf("tigerbeetle: %s", status)
}

// postNow posts one transfer atomically (single-phase).
func (e *Engine) postNow(typ, key string, src, dst uuid.UUID, amount int64) (store.Transfer, error) {
	s, _, err := e.prepare(typ, src, dst, amount)
	if err != nil {
		return store.Transfer{}, err
	}
	id := keyID(key)
	t := tb.Transfer{
		ID: toTB(id), DebitAccountID: toTB(src), CreditAccountID: toTB(dst),
		Amount: u128(amount), Ledger: ledgerID, Code: typeCodes[typ],
	}
	if err := e.submitDebit(t, key, guarded(typ, s), src, amount); err != nil {
		return store.Transfer{}, err
	}
	return e.GetTransfer(context.Background(), id)
}

// guarded reports whether a debit must pass the receivable guard: every
// Wallet debit except a Repayment (A§9.7).
func guarded(typ string, src tb.Account) bool {
	return src.Code == codeWallet && typ != ledger.TypeRepayment
}

// submitDebit sends t. When guarded, t is linked with a balancing_credit from
// control D into the debtor's receivable. That moves exactly what's owed, and D
// can't go negative, so the chain fails iff something is owed
// (prototypes/tb-gaps).
func (e *Engine) submitDebit(t tb.Transfer, key string, guard bool, src uuid.UUID, amount int64) error {
	if !guard {
		st, err := e.submit(t)
		if err != nil {
			return err
		}
		return e.outcome(st[0], src, amount)
	}
	t.Flags |= tb.TransferFlags{Linked: true}.ToUint16()
	st, err := e.submit(t, tb.Transfer{
		ID: toTB(derive("guard", key)), DebitAccountID: toTB(e.guard), CreditAccountID: toTB(receivableID(src)),
		Amount: tb.AmountMax, Ledger: ledgerID, Code: codeGuard,
		Flags: tb.TransferFlags{BalancingCredit: true}.ToUint16(),
	})
	if err != nil {
		return err
	}
	switch {
	case st[0] == tb.TransferExists:
		return nil // a replay of a debit that already succeeded
	case st[0] == tb.TransferLinkedEventFailed && st[1] == tb.TransferExceedsCredits:
		owed, err := e.owed(src)
		if err != nil {
			return err
		}
		return &ledger.ReceivableOpenError{Owed: owed}
	case st[0] == tb.TransferLinkedEventFailed && st[1] != tb.TransferCreated:
		return fmt.Errorf("tigerbeetle receivable guard: %s", st[1])
	}
	return e.outcome(st[0], src, amount)
}

func (e *Engine) CreateTransfer(_ context.Context, r ledger.AcceptRequest) (store.Transfer, error) {
	return e.postNow(ledger.TypeP2P, r.Key, r.SourceID, r.DestID, r.Amount)
}

func (e *Engine) TopUp(_ context.Context, key string, _ []byte, wallet uuid.UUID, amount int64) (store.Transfer, error) {
	return e.postNow(ledger.TypeTopUp, key, e.funding, wallet, amount)
}

func (e *Engine) Withdraw(_ context.Context, key string, _ []byte, wallet uuid.UUID, amount int64) (store.Transfer, error) {
	return e.postNow(ledger.TypeWithdrawal, key, wallet, e.funding, amount)
}

// ---- holds ----

// timeoutSeconds rounds a TTL up to TigerBeetle's whole seconds (A§9.7).
func timeoutSeconds(ttl time.Duration) uint32 {
	return uint32(math.Ceil(ttl.Seconds()))
}

func (e *Engine) PlaceHold(ctx context.Context, r ledger.AcceptRequest) (store.Transfer, store.Hold, error) {
	s, _, err := e.prepare(ledger.TypeP2P, r.SourceID, r.DestID, r.Amount)
	if err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	ttl := r.HoldTTL
	if ttl == 0 {
		ttl = e.holdTTL
	}
	id := keyID(r.Key)
	t := tb.Transfer{
		ID: toTB(id), DebitAccountID: toTB(r.SourceID), CreditAccountID: toTB(r.DestID),
		Amount: u128(r.Amount), Ledger: ledgerID, Code: typeCodes[ledger.TypeP2P],
		Timeout: timeoutSeconds(ttl), Flags: tb.TransferFlags{Pending: true}.ToUint16(),
	}
	if err := e.submitDebit(t, r.Key, guarded(ledger.TypeP2P, s), r.SourceID, r.Amount); err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	return e.holdState(ctx, id)
}

// CaptureHold posts amount (all of it when nil) from a pending transfer; the
// remainder returns to the source. The post's id derives from the Hold, so a
// second capture with any key replays the current state (A§9.7 parity limit).
func (e *Engine) CaptureHold(ctx context.Context, _ string, _ []byte, holdID uuid.UUID, amount *int64) (store.Transfer, store.Hold, error) {
	p, err := e.pending(holdID)
	if err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	amt := amountOf(p.Amount)
	if amount != nil {
		if *amount <= 0 || *amount > amt {
			return store.Transfer{}, store.Hold{}, invalid(fmt.Sprintf("amount must be between 1 and the hold amount %d.", amt))
		}
		amt = *amount
	}
	st, err := e.submit(tb.Transfer{
		ID: toTB(postID(holdID)), PendingID: p.ID, DebitAccountID: p.DebitAccountID, CreditAccountID: p.CreditAccountID,
		Amount: u128(amt), Ledger: ledgerID, Code: p.Code, Flags: tb.TransferFlags{PostPendingTransfer: true}.ToUint16(),
	})
	if err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	if err := e.settleOutcome(st[0], p); err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	return e.holdState(ctx, holdID)
}

func (e *Engine) ReleaseHold(ctx context.Context, _ string, _ []byte, holdID uuid.UUID) (store.Hold, error) {
	p, err := e.pending(holdID)
	if err != nil {
		return store.Hold{}, err
	}
	st, err := e.submit(tb.Transfer{
		ID: toTB(voidID(holdID)), PendingID: p.ID, DebitAccountID: p.DebitAccountID, CreditAccountID: p.CreditAccountID,
		Amount: p.Amount, Ledger: ledgerID, Code: p.Code, Flags: tb.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
	})
	if err != nil {
		return store.Hold{}, err
	}
	if err := e.settleOutcome(st[0], p); err != nil {
		return store.Hold{}, err
	}
	_, h, err := e.holdState(ctx, holdID)
	return h, err
}

// pending loads a manual Hold's pending transfer.
func (e *Engine) pending(holdID uuid.UUID) (tb.Transfer, error) {
	ts, err := e.c.LookupTransfers([]tb.Uint128{toTB(holdID)})
	if err != nil {
		return tb.Transfer{}, fmt.Errorf("lookup hold: %w", err)
	}
	if len(ts) == 0 {
		return tb.Transfer{}, &ledger.NotFoundError{Resource: "hold", ID: holdID.String()}
	}
	if !ts[0].TransferFlags().Pending {
		return tb.Transfer{}, invalid("hold_id belongs to a transfer that is captured automatically.")
	}
	return ts[0], nil
}

// settleOutcome maps a post/void status onto the Hold errors.
func (e *Engine) settleOutcome(status tb.CreateTransferStatus, p tb.Transfer) error {
	switch status {
	case tb.TransferCreated, tb.TransferExists:
		return nil
	case tb.TransferPendingTransferExpired:
		return &ledger.HoldExpiredError{ExpiresAt: at(p.Timestamp + uint64(p.Timeout)*uint64(time.Second))}
	case tb.TransferPendingTransferAlreadyPosted:
		return &ledger.HoldNotActiveError{Status: "captured"}
	case tb.TransferPendingTransferAlreadyVoided:
		return &ledger.HoldNotActiveError{Status: "released"}
	}
	if existsDifferently(status) { // e.g. a second capture for a different amount
		return &ledger.HoldNotActiveError{Status: "captured"}
	}
	return fmt.Errorf("tigerbeetle: %s", status)
}

// holdState reads a pending transfer and its settlement into a Transfer and a
// Hold view, the way the Postgres ledger reports them.
func (e *Engine) holdState(_ context.Context, holdID uuid.UUID) (store.Transfer, store.Hold, error) {
	ts, err := e.c.LookupTransfers([]tb.Uint128{toTB(holdID), toTB(postID(holdID)), toTB(voidID(holdID))})
	if err != nil {
		return store.Transfer{}, store.Hold{}, fmt.Errorf("lookup hold: %w", err)
	}
	var p, post, void *tb.Transfer
	for i := range ts {
		switch fromTB(ts[i].ID) {
		case holdID:
			p = &ts[i]
		case postID(holdID):
			post = &ts[i]
		case voidID(holdID):
			void = &ts[i]
		}
	}
	if p == nil {
		return store.Transfer{}, store.Hold{}, &ledger.NotFoundError{Resource: "hold", ID: holdID.String()}
	}
	t := transferView(*p)
	h := store.Hold{
		ID: holdID, TransferID: holdID, SourceID: t.SourceID, DestID: t.DestID,
		Amount: t.Amount, Status: "active", CaptureMode: ledger.CaptureManual,
	}
	if p.Timeout > 0 {
		exp := at(p.Timestamp + uint64(p.Timeout)*uint64(time.Second))
		h.ExpiresAt = &exp
	}
	switch {
	case post != nil:
		captured := amountOf(post.Amount)
		h.Status, h.CapturedAmount, t.Status = "captured", &captured, "posted"
		postedAt := at(post.Timestamp)
		t.PostedAt = &postedAt
	case void != nil:
		h.Status, t.Status = "released", "failed"
	case h.ExpiresAt != nil && time.Now().After(*h.ExpiresAt):
		h.Status, t.Status = "expired", "failed"
	default:
		t.Status = "pending"
	}
	return t, h, nil
}

// transferView converts a TigerBeetle transfer. Single-phase transfers are
// posted the moment they exist.
func transferView(t tb.Transfer) store.Transfer {
	created := at(t.Timestamp)
	return store.Transfer{
		ID: fromTB(t.ID), Type: typeOf(t.Code), SourceID: fromTB(t.DebitAccountID), DestID: fromTB(t.CreditAccountID),
		Amount: amountOf(t.Amount), Status: "posted", CreatedAt: created, PostedAt: &created,
	}
}

// ---- reads ----

func (e *Engine) GetTransfer(ctx context.Context, id uuid.UUID) (store.Transfer, error) {
	ts, err := e.c.LookupTransfers([]tb.Uint128{toTB(id)})
	if err != nil {
		return store.Transfer{}, fmt.Errorf("lookup transfer: %w", err)
	}
	if len(ts) == 0 {
		return store.Transfer{}, &ledger.NotFoundError{Resource: "transfer", ID: id.String()}
	}
	if ts[0].TransferFlags().Pending {
		t, _, err := e.holdState(ctx, id)
		return t, err
	}
	return e.view(ts[0])
}

func (e *Engine) GetBalance(ctx context.Context, accountID uuid.UUID) (ledger.Balance, error) {
	accts, err := e.lookupAccounts(accountID, receivableID(accountID))
	if err != nil {
		return ledger.Balance{}, err
	}
	a, ok := accts[accountID]
	if !ok {
		return ledger.Balance{}, &ledger.NotFoundError{Resource: "account", ID: accountID.String()}
	}
	b := ledger.Balance{Posted: posted(a), Available: available(a)}
	if r, ok := accts[receivableID(accountID)]; ok {
		b.ReceivableOwed = posted(r) + amountOf(r.DebitsPending)
	}
	// Active Holds: this account's pending debits that haven't settled.
	ts, err := e.c.GetAccountTransfers(tb.AccountFilter{
		AccountID: toTB(accountID), Limit: 8189,
		Flags: tb.AccountFilterFlags{Debits: true, Reversed: true}.ToUint32(),
	})
	if err != nil {
		return ledger.Balance{}, fmt.Errorf("account transfers: %w", err)
	}
	for _, t := range ts {
		if !t.TransferFlags().Pending {
			continue
		}
		if _, h, err := e.holdState(ctx, fromTB(t.ID)); err == nil && h.Status == "active" {
			b.ActiveHolds = append(b.ActiveHolds, h)
		}
	}
	return b, nil
}

// GetBalanceAt reads the balance history (accounts are created with
// flags.history).
func (e *Engine) GetBalanceAt(_ context.Context, accountID uuid.UUID, t time.Time) (int64, error) {
	accts, err := e.lookupAccounts(accountID)
	if err != nil {
		return 0, err
	}
	a, ok := accts[accountID]
	if !ok {
		return 0, &ledger.NotFoundError{Resource: "account", ID: accountID.String()}
	}
	bs, err := e.c.GetAccountBalances(tb.AccountFilter{
		AccountID: toTB(accountID), TimestampMax: nanos(t), Limit: 1,
		Flags: tb.AccountFilterFlags{Debits: true, Credits: true, Reversed: true}.ToUint32(),
	})
	if err != nil {
		return 0, fmt.Errorf("account balances: %w", err)
	}
	if len(bs) == 0 {
		return 0, nil
	}
	a.DebitsPosted, a.CreditsPosted = bs[0].DebitsPosted, bs[0].CreditsPosted
	return posted(a), nil
}

// ListEntries pages an account's posted movements oldest first. TigerBeetle
// has no per-account version, so the transfer timestamp (unique and
// increasing) serves as both the version and the cursor.
func (e *Engine) ListEntries(_ context.Context, accountID uuid.UUID, from, to *time.Time, after int64, limit int) ([]store.EntryRow, bool, error) {
	accts, err := e.lookupAccounts(accountID)
	if err != nil {
		return nil, false, err
	}
	a, ok := accts[accountID]
	if !ok {
		return nil, false, &ledger.NotFoundError{Resource: "account", ID: accountID.String()}
	}
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, ledger.MaxPage)
	filter := tb.AccountFilter{
		AccountID: toTB(accountID), TimestampMin: uint64(after) + 1, Limit: 8189,
		Flags: tb.AccountFilterFlags{Debits: true, Credits: true}.ToUint32(),
	}
	if from != nil && nanos(*from) > filter.TimestampMin {
		filter.TimestampMin = nanos(*from)
	}
	if to != nil {
		filter.TimestampMax = nanos(*to) - 1
	}
	ts, err := e.c.GetAccountTransfers(filter)
	if err != nil {
		return nil, false, fmt.Errorf("account transfers: %w", err)
	}
	bs, err := e.c.GetAccountBalances(filter)
	if err != nil {
		return nil, false, fmt.Errorf("account balances: %w", err)
	}
	balanceAt := make(map[uint64]tb.AccountBalance, len(bs))
	for _, b := range bs {
		balanceAt[b.Timestamp] = b
	}
	var out []store.EntryRow
	for _, t := range ts {
		f := t.TransferFlags()
		if f.Pending || f.VoidPendingTransfer {
			continue // reservations and their releases move no posted money
		}
		transferID := fromTB(t.ID)
		if f.PostPendingTransfer {
			transferID = fromTB(t.PendingID) // the Hold's Transfer
		}
		dir := "credit"
		if fromTB(t.DebitAccountID) == accountID {
			dir = "debit"
		}
		b := a
		b.DebitsPosted, b.CreditsPosted = balanceAt[t.Timestamp].DebitsPosted, balanceAt[t.Timestamp].CreditsPosted
		out = append(out, store.EntryRow{
			Entry: store.Entry{
				TransferID: transferID, AccountID: accountID, Direction: dir,
				Amount: amountOf(t.Amount), BalanceAfter: posted(b), AccountVersion: int64(t.Timestamp),
			},
			ID: fromTB(t.ID), CreatedAt: at(t.Timestamp),
		})
		if len(out) > limit {
			return out[:limit], true, nil
		}
	}
	return out, false, nil
}
