// Package ledger is a double-entry, idempotent ledger. Amounts are int64 cents.
// Sign convention: debit > 0, credit < 0, so every entry sums to zero and the
// sum of all account balances is always zero.
package ledger

import (
	"fmt"
	"sync"
	"time"
)

type Account string

const (
	Cash               Account = "cash"
	AdvancesReceivable Account = "advances_receivable"
	FeeRevenue         Account = "fee_revenue"
	FreelancerPayable  Account = "freelancer_payable"
	CreditLosses       Account = "credit_losses"
)

type Posting struct {
	Account Account `json:"account"`
	Cents   int64   `json:"cents"`
}

type Entry struct {
	Key      string    `json:"key"` // idempotency key
	Memo     string    `json:"memo"`
	Postings []Posting `json:"postings"`
	At       time.Time `json:"at"`
}

type Ledger struct {
	mu       sync.Mutex
	seen     map[string]bool
	entries  []Entry
	balances map[Account]int64
}

func New() *Ledger {
	return &Ledger{seen: map[string]bool{}, balances: map[Account]int64{}}
}

// Post applies an entry once. A repeated key is a no-op (applied=false).
func (l *Ledger) Post(e Entry) (applied bool, err error) {
	var sum int64
	for _, p := range e.Postings {
		sum += p.Cents
	}
	if sum != 0 {
		return false, fmt.Errorf("ledger: entry %q unbalanced by %d", e.Key, sum)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[e.Key] {
		return false, nil
	}
	l.seen[e.Key] = true
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	l.entries = append(l.entries, e)
	for _, p := range e.Postings {
		l.balances[p.Account] += p.Cents
	}
	return true, nil
}

// Replay re-applies persisted entries in order (used when restoring state).
func (l *Ledger) Replay(es []Entry) error {
	for _, e := range es {
		if _, err := l.Post(e); err != nil {
			return err
		}
	}
	return nil
}

func (l *Ledger) Balance(a Account) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[a]
}

// Balances returns every account, including zero-balance ones, so clients can
// render a stable table.
func (l *Ledger) Balances() map[Account]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[Account]int64{Cash: 0, AdvancesReceivable: 0, FeeRevenue: 0, FreelancerPayable: 0, CreditLosses: 0}
	for k, v := range l.balances {
		out[k] = v
	}
	return out
}

// Entries returns a copy of all entries, oldest first.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Disburse: we pay the freelancer `advance` and book a receivable of advance+fee.
func Disburse(key string, advance, fee int64) Entry {
	return Entry{Key: key, Memo: "Advance paid to freelancer", Postings: []Posting{
		{AdvancesReceivable, advance + fee},
		{Cash, -advance},
		{FeeRevenue, -fee},
	}}
}

// Settle: the client paid `paid`; `owed` repays the advance, the rest is owed to the freelancer.
func Settle(key string, paid, owed int64) Entry {
	repay := owed
	if paid < owed {
		repay = paid
	}
	return Entry{Key: key, Memo: "Client paid the invoice", Postings: []Posting{
		{Cash, paid},
		{AdvancesReceivable, -repay},
		{FreelancerPayable, -(paid - repay)},
	}}
}

// PayRemainder: we pay the freelancer what is left after repayment.
func PayRemainder(key string, cents int64) Entry {
	return Entry{Key: key, Memo: "Remainder paid to freelancer", Postings: []Posting{
		{FreelancerPayable, cents},
		{Cash, -cents},
	}}
}

// WriteOff: an advanced invoice will not be paid, so the receivable becomes a loss.
func WriteOff(key string, owed int64) Entry {
	return Entry{Key: key, Memo: "Unpaid invoice written off", Postings: []Posting{
		{CreditLosses, owed},
		{AdvancesReceivable, -owed},
	}}
}
