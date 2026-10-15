package ledger

import (
	"sync"
	"testing"
)

func sumAll(l *Ledger) int64 {
	var s int64
	for _, v := range l.Balances() {
		s += v
	}
	return s
}

func TestIdempotentAndBalanced(t *testing.T) {
	l := New()
	if ok, err := l.Post(Disburse("d1", 900, 50)); !ok || err != nil {
		t.Fatalf("first post: %v %v", ok, err)
	}
	if ok, _ := l.Post(Disburse("d1", 900, 50)); ok {
		t.Fatal("duplicate key must be a no-op")
	}
	if got := l.Balance(AdvancesReceivable); got != 950 {
		t.Fatalf("receivable = %d, want 950", got)
	}
}

func TestUnbalancedRejected(t *testing.T) {
	l := New()
	if _, err := l.Post(Entry{Key: "x", Postings: []Posting{{Cash, 10}}}); err == nil {
		t.Fatal("expected unbalanced error")
	}
}

func TestFullLifecycleLeavesBooksClean(t *testing.T) {
	l := New()
	l.Post(Disburse("d", 90_000, 3_000))
	l.Post(Settle("s", 100_000, 93_000))
	l.Post(PayRemainder("p", 7_000))
	if l.Balance(AdvancesReceivable) != 0 || l.Balance(FreelancerPayable) != 0 {
		t.Fatalf("open balances: %v", l.Balances())
	}
	if l.Balance(Cash) != 3_000 || l.Balance(FeeRevenue) != -3_000 {
		t.Fatalf("cash/fee: %v", l.Balances())
	}
	if sumAll(l) != 0 {
		t.Fatalf("books do not sum to zero: %v", l.Balances())
	}
}

func TestWriteOff(t *testing.T) {
	l := New()
	l.Post(Disburse("d", 90_000, 3_000))
	l.Post(WriteOff("w", 93_000))
	if l.Balance(AdvancesReceivable) != 0 || l.Balance(CreditLosses) != 93_000 || sumAll(l) != 0 {
		t.Fatalf("balances: %v", l.Balances())
	}
}

func TestReplayRebuildsBalances(t *testing.T) {
	a := New()
	a.Post(Disburse("d", 500, 20))
	a.Post(Settle("s", 600, 520))
	b := New()
	if err := b.Replay(a.Entries()); err != nil {
		t.Fatal(err)
	}
	for k, v := range a.Balances() {
		if b.Balance(k) != v {
			t.Fatalf("%s: %d != %d", k, b.Balance(k), v)
		}
	}
}

func TestConcurrentPostsAreSafe(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Post(Disburse("same-key", 100, 5)) // 50 racers, one winner
		}()
	}
	wg.Wait()
	if l.Balance(AdvancesReceivable) != 105 {
		t.Fatalf("receivable = %d, want 105", l.Balance(AdvancesReceivable))
	}
}
