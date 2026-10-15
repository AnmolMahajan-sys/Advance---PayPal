package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"advance/internal/ai"
	"advance/internal/ledger"
	"advance/internal/paypal"
	"advance/internal/underwriting"
)

type fakePay struct {
	mu       sync.Mutex
	n        int32
	payouts  map[string]int64 // keyed by request ID, like PayPal idempotency
	rejectWH bool
	failPay  bool
}

func newFake() *fakePay { return &fakePay{payouts: map[string]int64{}} }

func (f *fakePay) CreateDraftInvoice(context.Context, paypal.InvoiceInput) (string, error) {
	return fmt.Sprintf("INV2-T%d", atomic.AddInt32(&f.n, 1)), nil
}
func (f *fakePay) SendInvoice(context.Context, string) error { return nil }
func (f *fakePay) PayerViewURL(context.Context, string) (string, error) {
	return "https://pay.test/x", nil
}
func (f *fakePay) CreatePayout(_ context.Context, id, _ string, cents int64, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPay {
		return "", fmt.Errorf("paypal down")
	}
	f.payouts[id] = cents
	return "BATCH-" + id, nil
}
func (f *fakePay) VerifyWebhook(context.Context, string, http.Header, []byte) (bool, error) {
	return !f.rejectWH, nil
}
func (f *fakePay) payoutCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.payouts) }

type stubReviewer struct{ res ai.Result }

func (s stubReviewer) Review(context.Context, ai.Input) (ai.Result, error) { return s.res, nil }

type env struct {
	srv *Server
	h   http.Handler
	pay *fakePay
	led *ledger.Ledger
}

func setup(t *testing.T, mut func(*Options), rv ai.Reviewer) env {
	t.Helper()
	pay, led := newFake(), ledger.New()
	opts := Options{WebhookID: "WH", DemoMode: true, MaxExposureCents: 10_000_000, DefaultAfter: 60 * 24 * time.Hour}
	if mut != nil {
		mut(&opts)
	}
	if rv == nil {
		rv = ai.RulesReviewer{}
	}
	s := New(pay, led, underwriting.RuleScorer{MaxAdvanceCents: 5_000_000}, rv, opts)
	return env{s, s.Routes(), pay, led}
}

func (e env) call(method, path string, body any, hdr ...string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e env) invoice(t *testing.T, client string, cents int64) string {
	t.Helper()
	rec := e.call("POST", "/invoices", map[string]any{
		"freelancer_email": "free@lance.test", "client_email": client, "amount_cents": cents, "description": "Brand design"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var inv Invoice
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	return inv.ID
}

func (e env) get(t *testing.T, id string) Invoice {
	t.Helper()
	var inv Invoice
	_ = json.Unmarshal(e.call("GET", "/invoices/"+id, nil).Body.Bytes(), &inv)
	return inv
}

func paidEvent(evID, inv string) map[string]any {
	return map[string]any{"id": evID, "event_type": "INVOICING.INVOICE.PAID",
		"resource": map[string]any{"invoice": map[string]any{"id": inv}}}
}

func booksBalance(l *ledger.Ledger) bool {
	var s int64
	for _, v := range l.Balances() {
		s += v
	}
	return s == 0
}

func TestAdvanceAndSettleFlow(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	if rec := e.call("POST", "/invoices/"+id+"/advance", nil); rec.Code != 200 {
		t.Fatalf("advance: %d %s", rec.Code, rec.Body)
	}
	if e.call("POST", "/invoices/"+id+"/advance", nil).Code != 409 {
		t.Fatal("second advance must be rejected")
	}
	inv := e.get(t, id)
	if inv.Status != StatusAdvanced || inv.Decision == nil || inv.Decision.Explanation == "" || inv.PayoutBatchID == "" {
		t.Fatalf("after advance: %+v", inv)
	}

	for i := 0; i < 3; i++ { // PayPal retries; same event ID must not double-post
		if rec := e.call("POST", "/webhooks/paypal", paidEvent("WH-1", id)); rec.Code != 200 {
			t.Fatalf("webhook: %d %s", rec.Code, rec.Body)
		}
	}
	// and a retry under a *new* event ID must still be idempotent at the ledger and payout level
	e.call("POST", "/webhooks/paypal", paidEvent("WH-2", id))

	inv = e.get(t, id)
	if inv.Status != StatusSettled || inv.ClosedAt == nil {
		t.Fatalf("status = %s", inv.Status)
	}
	if e.pay.payouts["adv-"+id] != inv.AdvanceCents || e.pay.payouts["rem-"+id] != 100_000-inv.AdvanceCents-inv.FeeCents {
		t.Fatalf("payouts: %v", e.pay.payouts)
	}
	if e.pay.payoutCount() != 2 {
		t.Fatalf("expected exactly 2 payouts, got %v", e.pay.payouts)
	}
	if e.led.Balance(ledger.AdvancesReceivable) != 0 || e.led.Balance(ledger.FreelancerPayable) != 0 || !booksBalance(e.led) {
		t.Fatalf("ledger not clean: %v", e.led.Balances())
	}
	if e.led.Balance(ledger.Cash) != inv.FeeCents {
		t.Fatalf("cash %d != fee %d", e.led.Balance(ledger.Cash), inv.FeeCents)
	}
}

func TestDeclines(t *testing.T) {
	for name, client := range map[string]string{"risky": "risky@example.com", "no history": "newclient@example.com", "unknown": "stranger@nowhere.test"} {
		e := setup(t, nil, nil)
		id := e.invoice(t, client, 50_000)
		rec := e.call("POST", "/invoices/"+id+"/advance", nil)
		if rec.Code != 200 || e.pay.payoutCount() != 0 || e.get(t, id).Status != StatusDeclined {
			t.Fatalf("%s: code=%d payouts=%v status=%s", name, rec.Code, e.pay.payouts, e.get(t, id).Status)
		}
		if e.get(t, id).Decision.Explanation == "" {
			t.Fatalf("%s: a decline must explain itself", name)
		}
	}
}

func TestSelfInvoiceDeclined(t *testing.T) {
	e := setup(t, nil, nil)
	rec := e.call("POST", "/invoices", map[string]any{"freelancer_email": "me@x.test", "client_email": "ME@x.test", "amount_cents": 10_000})
	var inv Invoice
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	e.srv.histories["me@x.test"] = underwriting.ClientHistory{Paid: 50}
	e.call("POST", "/invoices/"+inv.ID+"/advance", nil)
	if e.pay.payoutCount() != 0 {
		t.Fatal("must not advance against an invoice you sent to yourself")
	}
}

func TestAIReviewCanOnlyTighten(t *testing.T) {
	e := setup(t, nil, stubReviewer{ai.Result{Verdict: ai.VerdictReduce, Multiplier: 0.5, Explanation: "Big for this client.", Source: "claude"}})
	id := e.invoice(t, "reliable@example.com", 100_000)
	rule := underwriting.RuleScorer{}.Score(underwriting.Request{AmountCents: 100_000, FreelancerEmail: "a@b.c", ClientEmail: "reliable@example.com", History: e.srv.histories["reliable@example.com"]})
	e.call("POST", "/invoices/"+id+"/advance", nil)
	inv := e.get(t, id)
	if inv.AdvanceCents != rule.AdvanceCents/2 || inv.Decision.ReviewedBy != "claude" {
		t.Fatalf("advance=%d want %d decision=%+v", inv.AdvanceCents, rule.AdvanceCents/2, inv.Decision)
	}

	e = setup(t, nil, stubReviewer{ai.Result{Verdict: ai.VerdictReduce, Multiplier: 99, Explanation: "pay me more", Source: "claude"}})
	id = e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	if got := e.get(t, id).AdvanceCents; got != rule.AdvanceCents {
		t.Fatalf("a multiplier above 1 must be clamped: got %d, rule allows %d", got, rule.AdvanceCents)
	}
}

func TestAIDeclineStopsPayout(t *testing.T) {
	e := setup(t, nil, stubReviewer{ai.Result{Verdict: ai.VerdictDecline, Explanation: "Looks fabricated.", RedFlags: []string{"vague"}, Source: "claude"}})
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	if e.pay.payoutCount() != 0 || e.get(t, id).Status != StatusDeclined {
		t.Fatal("AI decline must stop the payout")
	}
}

func TestExposureCap(t *testing.T) {
	e := setup(t, func(o *Options) { o.MaxExposureCents = 100_000 }, nil)
	a, b := e.invoice(t, "reliable@example.com", 100_000), e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+a+"/advance", nil)
	e.call("POST", "/invoices/"+b+"/advance", nil)
	if e.get(t, a).Status != StatusAdvanced || e.get(t, b).Status != StatusDeclined {
		t.Fatalf("a=%s b=%s", e.get(t, a).Status, e.get(t, b).Status)
	}
}

func TestConcurrentAdvanceMovesMoneyOnce(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.call("POST", "/invoices/"+id+"/advance", nil).Code == 200 {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || e.pay.payoutCount() != 1 {
		t.Fatalf("successes=%d payouts=%v", ok, e.pay.payouts)
	}
}

func TestPayoutFailureRollsBack(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.pay.failPay = true
	if rec := e.call("POST", "/invoices/"+id+"/advance", nil); rec.Code != 502 {
		t.Fatalf("code=%d", rec.Code)
	}
	if e.get(t, id).Status != StatusSent || e.led.Balance(ledger.AdvancesReceivable) != 0 || e.srv.reserved != 0 {
		t.Fatalf("state not rolled back: %s %v reserved=%d", e.get(t, id).Status, e.led.Balances(), e.srv.reserved)
	}
	e.pay.failPay = false
	if e.call("POST", "/invoices/"+id+"/advance", nil).Code != 200 {
		t.Fatal("retry after failure should work")
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	e.pay.rejectWH = true
	if rec := e.call("POST", "/webhooks/paypal", paidEvent("WH-9", id)); rec.Code != 401 {
		t.Fatalf("code=%d", rec.Code)
	}
	if e.get(t, id).Status != StatusAdvanced {
		t.Fatal("forged webhook must not settle anything")
	}
}

func TestPaidWithoutAdvanceIsIgnored(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	if rec := e.call("POST", "/webhooks/paypal", paidEvent("WH-3", id)); rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if e.pay.payoutCount() != 0 || len(e.led.Entries()) != 0 {
		t.Fatal("an invoice we never advanced is not ours to settle")
	}
}

func TestCancelledAfterAdvanceIsWrittenOffAndWorsensHistory(t *testing.T) {
	e := setup(t, nil, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	before := e.srv.histories["reliable@example.com"].Defaulted
	ev := map[string]any{"id": "WH-C", "event_type": "INVOICING.INVOICE.CANCELLED", "resource": map[string]any{"invoice": map[string]any{"id": id}}}
	e.call("POST", "/webhooks/paypal", ev)
	e.call("POST", "/webhooks/paypal", map[string]any{"id": "WH-C2", "event_type": ev["event_type"], "resource": ev["resource"]})
	inv := e.get(t, id)
	if inv.Status != StatusDefaulted || e.led.Balance(ledger.CreditLosses) != inv.AdvanceCents+inv.FeeCents || !booksBalance(e.led) {
		t.Fatalf("status=%s ledger=%v", inv.Status, e.led.Balances())
	}
	if e.srv.histories["reliable@example.com"].Defaulted != before+1 {
		t.Fatal("a default must be recorded exactly once")
	}
}

func TestSettlementImprovesClientRecord(t *testing.T) {
	e := setup(t, nil, nil)
	before := e.srv.histories["slow@example.com"].Paid
	id := e.invoice(t, "slow@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	e.call("POST", "/demo/invoices/"+id+"/pay", nil)
	e.call("POST", "/demo/invoices/"+id+"/pay", nil)
	if e.srv.histories["slow@example.com"].Paid != before+1 {
		t.Fatalf("paid count = %d, want %d", e.srv.histories["slow@example.com"].Paid, before+1)
	}
}

func TestSweepDefaults(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e := setup(t, func(o *Options) { o.Now = func() time.Time { return now } }, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	if n := e.srv.SweepDefaults(); n != 0 {
		t.Fatalf("nothing is overdue yet, swept %d", n)
	}
	now = now.Add(61 * 24 * time.Hour)
	if n := e.srv.SweepDefaults(); n != 1 || e.get(t, id).Status != StatusDefaulted {
		t.Fatalf("swept=%d status=%s", n, e.get(t, id).Status)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	e := setup(t, func(o *Options) { o.DataFile = file }, nil)
	id := e.invoice(t, "reliable@example.com", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	want := e.led.Balances()

	e2 := setup(t, func(o *Options) { o.DataFile = file }, nil)
	if err := e2.srv.LoadState(); err != nil {
		t.Fatal(err)
	}
	inv := e2.get(t, id)
	if inv.Status != StatusAdvanced || inv.Decision == nil {
		t.Fatalf("restored invoice: %+v", inv)
	}
	for k, v := range want {
		if e2.led.Balance(k) != v {
			t.Fatalf("%s: %d != %d", k, e2.led.Balance(k), v)
		}
	}
	e2.call("POST", "/webhooks/paypal", paidEvent("WH-P", id))
	if e2.get(t, id).Status != StatusSettled {
		t.Fatal("a restored invoice must still settle")
	}
}

func TestValidation(t *testing.T) {
	e := setup(t, nil, nil)
	bad := []map[string]any{
		{"freelancer_email": "nope", "client_email": "c@x.test", "amount_cents": 1000},
		{"freelancer_email": "f@x.test", "client_email": "", "amount_cents": 1000},
		{"freelancer_email": "f@x.test", "client_email": "c@x.test", "amount_cents": 0},
		{"freelancer_email": "f@x.test", "client_email": "c@x.test", "amount_cents": 99_999_999},
		{"freelancer_email": "f@x.test", "client_email": "c@x.test", "amount_cents": 1000, "currency": "DOLLARS"},
	}
	for i, b := range bad {
		if rec := e.call("POST", "/invoices", b); rec.Code != 400 {
			t.Errorf("case %d: code=%d", i, rec.Code)
		}
	}
	if rec := e.call("POST", "/invoices/missing/advance", nil); rec.Code != 404 {
		t.Errorf("unknown invoice: %d", rec.Code)
	}
}

func TestAPIKeyAndDemoGate(t *testing.T) {
	e := setup(t, func(o *Options) { o.APIKey = "s3cret" }, nil)
	if e.call("GET", "/invoices", nil).Code != 401 {
		t.Fatal("API must require the key")
	}
	if e.call("GET", "/invoices", nil, "X-API-Key", "s3cret").Code != 200 {
		t.Fatal("valid key must pass")
	}
	if e.call("GET", "/invoices", nil, "Authorization", "Bearer s3cret").Code != 200 {
		t.Fatal("bearer form must pass")
	}
	if e.call("GET", "/healthz", nil).Code != 200 || e.call("GET", "/meta", nil).Code != 200 {
		t.Fatal("health and meta stay public")
	}

	prod := setup(t, func(o *Options) { o.DemoMode = false }, nil)
	if prod.call("POST", "/demo/invoices/x/pay", nil).Code != 404 {
		t.Fatal("demo endpoints must not exist outside demo mode")
	}
}

func TestRateLimiter(t *testing.T) {
	l := &limiter{rate: 1, burst: 3, m: map[string]*bucket{}}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allow("ip", now) {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if l.allow("ip", now) {
		t.Fatal("4th immediate request must be refused")
	}
	if !l.allow("other", now) {
		t.Fatal("limits are per client")
	}
	if !l.allow("ip", now.Add(1100*time.Millisecond)) {
		t.Fatal("tokens must refill over time")
	}
}

func TestExtraClientGetsAProfile(t *testing.T) {
	e := setup(t, func(o *Options) {
		o.ExtraClients = map[string]string{"Buyer@Sandbox.test": "reliable", "x@y.test": "bogus"}
	}, nil)
	id := e.invoice(t, "buyer@sandbox.test", 100_000)
	e.call("POST", "/invoices/"+id+"/advance", nil)
	if e.get(t, id).Status != StatusAdvanced {
		t.Fatalf("a sandbox buyer mapped to the reliable profile should be advanced, got %s", e.get(t, id).Status)
	}
	var meta struct{ Clients []SeedClient }
	_ = json.Unmarshal(e.call("GET", "/meta", nil).Body.Bytes(), &meta)
	if len(meta.Clients) != len(seedClients)+1 {
		t.Fatalf("meta should list the extra client once, got %d", len(meta.Clients))
	}
}
