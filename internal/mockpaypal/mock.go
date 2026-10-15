// Package mockpaypal is a tiny stand-in for the PayPal REST endpoints Advance
// uses, so the whole flow can be run and tested offline. It is a development
// tool, not a PayPal emulator: response shapes follow PayPal's docs but only
// the fields Advance reads are populated.
package mockpaypal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type Mock struct {
	mu            sync.Mutex
	invoices      map[string]string // id -> amount value
	payouts       map[string]Payout // request id -> payout
	seq, evSeq    int
	webhookTarget string
	baseURL       string
}

type Payout struct {
	RequestID string `json:"request_id"`
	Receiver  string `json:"receiver"`
	Amount    string `json:"amount"`
	BatchID   string `json:"batch_id"`
}

func New(webhookTarget string) *Mock {
	return &Mock{invoices: map[string]string{}, payouts: map[string]Payout{}, webhookTarget: webhookTarget}
}

func (m *Mock) SetWebhookTarget(u string) { m.mu.Lock(); m.webhookTarget = u; m.mu.Unlock() }

// SendEvent delivers a webhook to Advance, as PayPal would.
func (m *Mock) SendEvent(eventType, invoiceID string) error {
	m.mu.Lock()
	m.evSeq++
	id, target := fmt.Sprintf("WH-MOCK-%d", m.evSeq), m.webhookTarget
	m.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"id": id, "event_type": eventType,
		"resource": map[string]any{"invoice": map[string]any{"id": invoiceID}},
	})
	resp, err := http.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("advance answered %s", resp.Status)
	}
	return nil
}

func (m *Mock) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		js(w, 200, map[string]any{"access_token": "mock-token", "token_type": "Bearer", "expires_in": 32400})
	})
	mux.HandleFunc("POST /v2/invoicing/invoices", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Items []struct {
				UnitAmount struct {
					Value string `json:"value"`
				} `json:"unit_amount"`
			} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		m.mu.Lock()
		m.seq++
		id := fmt.Sprintf("INV2-MOCK-%04d", m.seq)
		amt := "0.00"
		if len(b.Items) > 0 {
			amt = b.Items[0].UnitAmount.Value
		}
		m.invoices[id] = amt
		m.mu.Unlock()
		js(w, 201, map[string]any{"rel": "self", "href": m.base(r) + "/v2/invoicing/invoices/" + id, "method": "GET"})
	})
	mux.HandleFunc("GET /v2/invoicing/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m.mu.Lock()
		_, ok := m.invoices[id]
		m.mu.Unlock()
		if !ok {
			js(w, 404, map[string]string{"name": "RESOURCE_NOT_FOUND"})
			return
		}
		js(w, 200, map[string]any{"id": id, "status": "SENT",
			"detail": map[string]any{"metadata": map[string]any{"recipient_view_url": m.base(r) + "/mock/pay/" + id}}})
	})
	mux.HandleFunc("POST /v2/invoicing/invoices/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		js(w, 202, map[string]any{"rel": "x", "href": m.base(r) + "/mock/pay/" + r.PathValue("id"), "method": "GET"})
	})
	mux.HandleFunc("POST /v1/payments/payouts", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Items []struct {
				Receiver string `json:"receiver"`
				Amount   struct {
					Value string `json:"value"`
				} `json:"amount"`
			} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		reqID := r.Header.Get("PayPal-Request-Id")
		m.mu.Lock()
		p, seen := m.payouts[reqID]
		if !seen && len(b.Items) > 0 {
			p = Payout{RequestID: reqID, Receiver: b.Items[0].Receiver, Amount: b.Items[0].Amount.Value, BatchID: fmt.Sprintf("MOCKBATCH-%d", len(m.payouts)+1)}
			m.payouts[reqID] = p
		}
		m.mu.Unlock()
		js(w, 201, map[string]any{"batch_header": map[string]any{"payout_batch_id": p.BatchID, "batch_status": "PENDING"}})
	})
	mux.HandleFunc("POST /v1/notifications/verify-webhook-signature", func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, map[string]string{"verification_status": "SUCCESS"})
	})

	// Dev helpers: a fake "pay this invoice" page and a payout ledger to assert against.
	mux.HandleFunc("GET /mock/pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := html.EscapeString(r.PathValue("id"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>Mock PayPal checkout</title>
<body style="font:16px system-ui;max-width:28rem;margin:4rem auto;padding:0 1rem">
<h1>Mock PayPal checkout</h1><p>Invoice <b>%s</b>. This page stands in for PayPal's hosted invoice page.</p>
<form method=post action="/mock/pay/%s"><button style="font:inherit;padding:.6rem 1rem">Pay invoice</button></form>`, id, id)
	})
	mux.HandleFunc("POST /mock/pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.SendEvent("INVOICING.INVOICE.PAID", r.PathValue("id")); err != nil {
			http.Error(w, "could not notify Advance: "+err.Error(), 502)
			return
		}
		fmt.Fprint(w, "Paid. Advance has been notified; you can close this tab.")
	})
	mux.HandleFunc("POST /mock/cancel/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.SendEvent("INVOICING.INVOICE.CANCELLED", r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		fmt.Fprint(w, "Cancelled.")
	})
	mux.HandleFunc("GET /mock/payouts", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		out := make([]Payout, 0, len(m.payouts))
		for _, p := range m.payouts {
			out = append(out, p)
		}
		m.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].BatchID < out[j].BatchID })
		js(w, 200, out)
	})
	return logging(mux)
}

func (m *Mock) base(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/mock/payouts") {
			log.Printf("mockpaypal: %s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
