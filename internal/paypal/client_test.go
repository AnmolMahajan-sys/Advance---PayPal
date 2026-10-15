package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFormatMoney(t *testing.T) {
	for in, want := range map[int64]string{0: "0.00", 5: "0.05", 100: "1.00", 123456: "1234.56"} {
		if got := FormatMoney(in); got != want {
			t.Errorf("FormatMoney(%d) = %s, want %s", in, got, want)
		}
	}
}

func fake(t *testing.T) (*Client, *int32, *httptest.Server) {
	t.Helper()
	var tokens int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokens, 1)
		u, p, _ := r.BasicAuth()
		if u != "id" || p != "secret" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("POST /v2/invoicing/invoices", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		items := b["items"].([]any)[0].(map[string]any)
		if items["unit_amount"].(map[string]any)["value"] != "125.50" {
			t.Errorf("amount not formatted: %v", items)
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"href": "https://x/v2/invoicing/invoices/INV2-ABC"})
	})
	mux.HandleFunc("GET /v2/invoicing/invoices/INV2-ABC", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"detail": map[string]any{"metadata": map[string]any{"recipient_view_url": "https://pay/me"}}})
	})
	mux.HandleFunc("POST /v1/payments/payouts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PayPal-Request-Id") != "req-1" {
			t.Errorf("missing idempotency header")
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"batch_header": map[string]any{"payout_batch_id": "B1"}})
	})
	mux.HandleFunc("POST /v1/notifications/verify-webhook-signature", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		st := "FAILURE"
		if b["transmission_id"] == "good" && b["webhook_id"] == "WH" {
			st = "SUCCESS"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"verification_status": st})
	})
	srv := httptest.NewServer(mux)
	return New(srv.URL, "id", "secret"), &tokens, srv
}

func TestInvoiceAndPayoutFlow(t *testing.T) {
	c, tokens, srv := fake(t)
	defer srv.Close()
	ctx := context.Background()

	id, err := c.CreateDraftInvoice(ctx, InvoiceInput{Currency: "USD", Description: "d", AmountCents: 12550, ClientEmail: "c@x.com"})
	if err != nil || id != "INV2-ABC" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if u, err := c.PayerViewURL(ctx, id); err != nil || u != "https://pay/me" {
		t.Fatalf("url=%q err=%v", u, err)
	}
	if b, err := c.CreatePayout(ctx, "req-1", "f@x.com", 100, "USD", "n"); err != nil || b != "B1" {
		t.Fatalf("batch=%q err=%v", b, err)
	}
	if n := atomic.LoadInt32(tokens); n != 1 {
		t.Fatalf("token should be fetched once and cached, fetched %d times", n)
	}
}

func TestBadCredentialsSurfaceAsError(t *testing.T) {
	_, _, srv := fake(t)
	defer srv.Close()
	c := New(srv.URL, "id", "wrong")
	if _, err := c.CreatePayout(context.Background(), "r", "a@b.c", 1, "USD", ""); err == nil {
		t.Fatal("expected oauth error")
	}
}

func TestVerifyWebhook(t *testing.T) {
	c, _, srv := fake(t)
	defer srv.Close()
	h := http.Header{}
	h.Set("PAYPAL-TRANSMISSION-ID", "good")
	if ok, err := c.VerifyWebhook(context.Background(), "WH", h, []byte(`{"id":"1"}`)); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	h.Set("PAYPAL-TRANSMISSION-ID", "forged")
	if ok, _ := c.VerifyWebhook(context.Background(), "WH", h, []byte(`{"id":"1"}`)); ok {
		t.Fatal("forged webhook must not verify")
	}
}
