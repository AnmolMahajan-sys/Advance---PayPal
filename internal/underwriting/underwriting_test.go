package underwriting

import "testing"

func req(amount int64, h ClientHistory) Request {
	return Request{AmountCents: amount, FreelancerEmail: "f@x.com", ClientEmail: "c@y.com", History: h}
}

func TestReliableBeatsRisky(t *testing.T) {
	s := RuleScorer{MaxAdvanceCents: 1_000_000}
	good := s.Score(req(100_000, ClientHistory{Paid: 12, Late: 1}))
	bad := s.Score(req(100_000, ClientHistory{Paid: 2, Late: 1, Defaulted: 2}))
	if !good.Approved || bad.Approved {
		t.Fatalf("good=%v bad=%v", good.Approved, bad.Approved)
	}
	if good.AdvanceCents+good.FeeCents >= 100_000 {
		t.Fatal("advance+fee must stay below the invoice amount")
	}
}

func TestRiskierClientPaysMore(t *testing.T) {
	s := RuleScorer{}
	a := s.Score(req(100_000, ClientHistory{Paid: 20}))
	b := s.Score(req(100_000, ClientHistory{Paid: 6, Late: 4, AvgDaysLate: 18}))
	if !a.Approved || !b.Approved {
		t.Fatalf("both should be approved: %v %v", a.Approved, b.Approved)
	}
	if b.FeeCents <= a.FeeCents || b.AdvanceCents >= a.AdvanceCents {
		t.Fatalf("slow payer must get a smaller advance and a higher fee: %+v vs %+v", a, b)
	}
}

func TestUnknownClientIsNotAdvanced(t *testing.T) {
	d := RuleScorer{}.Score(req(50_000, ClientHistory{}))
	if d.Approved || d.RiskScore < 0.6 {
		t.Fatalf("decision=%+v", d)
	}
}

func TestSelfInvoicingBlocked(t *testing.T) {
	q := req(50_000, ClientHistory{Paid: 30})
	q.ClientEmail = " F@X.com "
	if d := (RuleScorer{}).Score(q); d.Approved {
		t.Fatal("same-party invoice must not be advanced")
	}
}

func TestLimitAndBadAmount(t *testing.T) {
	s := RuleScorer{MaxAdvanceCents: 10_000}
	if s.Score(req(20_000, ClientHistory{Paid: 30})).Approved {
		t.Fatal("over the per-invoice limit")
	}
	if s.Score(req(0, ClientHistory{Paid: 30})).Approved {
		t.Fatal("zero amount")
	}
}
