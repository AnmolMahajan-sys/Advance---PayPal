// Package paypal is a small stdlib-only client for the PayPal REST APIs
// Advance needs: OAuth, Invoicing v2, Payouts and webhook verification.
package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL, clientID, secret string
	http                      *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func New(baseURL, clientID, secret string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		clientID: clientID,
		secret:   secret,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// FormatMoney renders minor units (cents) as PayPal's decimal string.
func FormatMoney(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/oauth2/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paypal oauth: %s: %s", resp.Status, b)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", err
	}
	c.token = t.AccessToken
	c.expires = time.Now().Add(time.Duration(t.ExpiresIn-60) * time.Second)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, in, out any) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("paypal %s %s: %s: %s", method, path, resp.Status, b)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

type money struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type InvoiceInput struct {
	Currency    string
	Description string
	AmountCents int64
	ClientEmail string
}

// CreateDraftInvoice creates a draft invoice and returns its ID.
func (c *Client) CreateDraftInvoice(ctx context.Context, in InvoiceInput) (string, error) {
	body := map[string]any{
		"detail": map[string]any{"currency_code": in.Currency},
		"primary_recipients": []map[string]any{
			{"billing_info": map[string]any{"email_address": in.ClientEmail}},
		},
		"items": []map[string]any{{
			"name":        in.Description,
			"quantity":    "1",
			"unit_amount": money{in.Currency, FormatMoney(in.AmountCents)},
		}},
	}
	var out struct {
		Href string `json:"href"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/invoicing/invoices", nil, body, &out); err != nil {
		return "", err
	}
	i := strings.LastIndex(out.Href, "/")
	if i < 0 || i == len(out.Href)-1 {
		return "", fmt.Errorf("paypal: unexpected invoice href %q", out.Href)
	}
	return out.Href[i+1:], nil
}

func (c *Client) SendInvoice(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v2/invoicing/invoices/"+id+"/send", nil,
		map[string]any{"send_to_recipient": true}, nil)
}

// CreatePayout sends money to an email. requestID makes the call idempotent.
func (c *Client) CreatePayout(ctx context.Context, requestID, email string, cents int64, currency, note string) (string, error) {
	body := map[string]any{
		"sender_batch_header": map[string]any{
			"sender_batch_id": requestID,
			"email_subject":   "You have a payment from Advance",
		},
		"items": []map[string]any{{
			"recipient_type": "EMAIL",
			"receiver":       email,
			"amount":         money{currency, FormatMoney(cents)},
			"note":           note,
			"sender_item_id": requestID,
		}},
	}
	var out struct {
		BatchHeader struct {
			ID string `json:"payout_batch_id"`
		} `json:"batch_header"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/payments/payouts",
		map[string]string{"PayPal-Request-Id": requestID}, body, &out)
	return out.BatchHeader.ID, err
}

// VerifyWebhook asks PayPal to validate a webhook's signature.
func (c *Client) VerifyWebhook(ctx context.Context, webhookID string, h http.Header, body []byte) (bool, error) {
	req := map[string]any{
		"auth_algo":         h.Get("PAYPAL-AUTH-ALGO"),
		"cert_url":          h.Get("PAYPAL-CERT-URL"),
		"transmission_id":   h.Get("PAYPAL-TRANSMISSION-ID"),
		"transmission_sig":  h.Get("PAYPAL-TRANSMISSION-SIG"),
		"transmission_time": h.Get("PAYPAL-TRANSMISSION-TIME"),
		"webhook_id":        webhookID,
		"webhook_event":     json.RawMessage(body),
	}
	var out struct {
		Status string `json:"verification_status"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", nil, req, &out); err != nil {
		return false, err
	}
	return out.Status == "SUCCESS", nil
}

// PayerViewURL fetches the link the client opens to pay a sent invoice.
func (c *Client) PayerViewURL(ctx context.Context, id string) (string, error) {
	var out struct {
		Detail struct {
			Metadata struct {
				RecipientViewURL string `json:"recipient_view_url"`
			} `json:"metadata"`
		} `json:"detail"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/invoicing/invoices/"+id, nil, nil, &out); err != nil {
		return "", err
	}
	return out.Detail.Metadata.RecipientViewURL, nil
}
