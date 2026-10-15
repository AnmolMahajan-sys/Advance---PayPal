// Package e2e runs the real PayPal client against the offline mock over HTTP,
// with the mock delivering webhooks back to Advance, exactly like production.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"advance/internal/ai"
	"advance/internal/api"
	"advance/internal/ledger"
	"advance/internal/mockpaypal"
	"advance/internal/paypal"
	"advance/internal/underwriting"
)

type stack struct {
	t      *testing.T
	adv    *httptest.Server
	mock   *httptest.Server
	ledger *ledger.Ledger
}

func start(t *testing.T) *stack {
	t.Helper()
	m := mockpaypal.New("")
	mockSrv := httptest.NewServer(m.Handler())
	led := ledger.New()
	srv := api.New(paypal.New(mockSrv.URL, "id", "secret"), led,
		underwriting.RuleScorer{MaxAdvanceCents: 5_000_000}, ai.RulesReviewer{},
		api.Options{WebhookID: "WH", DemoMode: true, MaxExposureCents: 10_000_000, DefaultAfter: time.Hour})
	advSrv := httptest.NewServer(srv.Mount(nil))
	m.SetWebhookTarget(advSrv.URL + "/webhooks/paypal")
	t.Cleanup(func() { advSrv.Close(); mockSrv.Close() })
	return &stack{t, advSrv, mockSrv, led}
}

func (s *stack) do(method, url string, body any, out any) int {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestInvoiceToAdvanceToSettlementThroughWebhook(t *testing.T) {
	s := start(t)

	var inv api.Invoice
	if c := s.do("POST", s.adv.URL+"/invoices", map[string]any{
		"freelancer_email": "freelancer@example.com", "client_email": "reliable@example.com",
		"amount_cents": 250_000, "description": "Website redesign"}, &inv); c != 201 {
		t.Fatalf("create: %d", c)
	}
	if !strings.HasPrefix(inv.ID, "INV2-MOCK-") || !strings.Contains(inv.PayURL, "/mock/pay/"+inv.ID) {
		t.Fatalf("invoice did not round-trip through the PayPal client: %+v", inv)
	}

	var res struct {
		Invoice api.Invoice `json:"invoice"`
	}
	if c := s.do("POST", s.adv.URL+"/invoices/"+inv.ID+"/advance", nil, &res); c != 200 || res.Invoice.Status != api.StatusAdvanced {
		t.Fatalf("advance: %d %+v", c, res.Invoice)
	}

	// The client pays on PayPal's page; PayPal (the mock) calls our webhook.
	resp, err := http.Post(s.mock.URL+"/mock/pay/"+inv.ID, "", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("pay: %v %v", err, resp)
	}

	var final api.Invoice
	s.do("GET", s.adv.URL+"/invoices/"+inv.ID, nil, &final)
	if final.Status != api.StatusSettled {
		t.Fatalf("status = %s", final.Status)
	}

	var payouts []mockpaypal.Payout
	s.do("GET", s.mock.URL+"/mock/payouts", nil, &payouts)
	if len(payouts) != 2 {
		t.Fatalf("expected an advance payout and a remainder payout, got %+v", payouts)
	}
	for _, p := range payouts {
		if p.Receiver != "freelancer@example.com" {
			t.Fatalf("payout went to %s", p.Receiver)
		}
	}

	var lv struct {
		Balanced bool  `json:"balanced"`
		Exposure int64 `json:"exposure_cents"`
	}
	s.do("GET", s.adv.URL+"/ledger", nil, &lv)
	if !lv.Balanced || lv.Exposure != 0 {
		t.Fatalf("ledger: %+v", lv)
	}
}

func TestDeclinedInvoiceMovesNoMoney(t *testing.T) {
	s := start(t)
	var inv api.Invoice
	s.do("POST", s.adv.URL+"/invoices", map[string]any{
		"freelancer_email": "f@example.com", "client_email": "risky@example.com", "amount_cents": 100_000}, &inv)
	s.do("POST", s.adv.URL+"/invoices/"+inv.ID+"/advance", nil, nil)
	var payouts []mockpaypal.Payout
	s.do("GET", s.mock.URL+"/mock/payouts", nil, &payouts)
	if len(payouts) != 0 {
		t.Fatalf("declined invoice paid out: %+v", payouts)
	}
}

func TestCancelledInvoiceIsWrittenOff(t *testing.T) {
	s := start(t)
	var inv api.Invoice
	s.do("POST", s.adv.URL+"/invoices", map[string]any{
		"freelancer_email": "f@example.com", "client_email": "reliable@example.com", "amount_cents": 100_000}, &inv)
	s.do("POST", s.adv.URL+"/invoices/"+inv.ID+"/advance", nil, nil)
	if c := s.do("POST", s.mock.URL+"/mock/cancel/"+inv.ID, nil, nil); c != 200 {
		t.Fatalf("cancel: %d", c)
	}
	var final api.Invoice
	s.do("GET", s.adv.URL+"/invoices/"+inv.ID, nil, &final)
	if final.Status != api.StatusDefaulted || s.ledger.Balance(ledger.CreditLosses) == 0 {
		t.Fatalf("status=%s ledger=%v", final.Status, s.ledger.Balances())
	}
}
