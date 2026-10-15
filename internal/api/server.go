// Package api exposes Advance over HTTP: create invoices, request an advance,
// and receive PayPal webhooks that settle or write off advanced invoices.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"advance/internal/ai"
	"advance/internal/ledger"
	"advance/internal/paypal"
	"advance/internal/underwriting"
)

type Payments interface {
	CreateDraftInvoice(ctx context.Context, in paypal.InvoiceInput) (string, error)
	SendInvoice(ctx context.Context, id string) error
	PayerViewURL(ctx context.Context, id string) (string, error)
	CreatePayout(ctx context.Context, requestID, email string, cents int64, currency, note string) (string, error)
	VerifyWebhook(ctx context.Context, webhookID string, h http.Header, body []byte) (bool, error)
}

// Invoice statuses.
const (
	StatusSent      = "sent"
	StatusAdvancing = "advancing"
	StatusDeclined  = "declined"
	StatusAdvanced  = "advanced"
	StatusSettling  = "settling"
	StatusSettled   = "settled"
	StatusDefaulted = "defaulted"
	StatusCancelled = "cancelled"
)

type Invoice struct {
	ID              string                 `json:"id"`
	FreelancerEmail string                 `json:"freelancer_email"`
	ClientEmail     string                 `json:"client_email"`
	Description     string                 `json:"description"`
	AmountCents     int64                  `json:"amount_cents"`
	Currency        string                 `json:"currency"`
	Status          string                 `json:"status"`
	PayURL          string                 `json:"pay_url,omitempty"`
	Decision        *underwriting.Decision `json:"decision,omitempty"`
	AdvanceCents    int64                  `json:"advance_cents,omitempty"`
	FeeCents        int64                  `json:"fee_cents,omitempty"`
	PayoutBatchID   string                 `json:"payout_batch_id,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	AdvancedAt      *time.Time             `json:"advanced_at,omitempty"`
	ClosedAt        *time.Time             `json:"closed_at,omitempty"`
}

type Options struct {
	WebhookID        string
	DemoMode         bool
	APIKey           string
	MaxExposureCents int64
	DefaultAfter     time.Duration
	DataFile         string
	// ExtraClients maps an email (for example your PayPal sandbox buyer account) to a
	// demo profile: reliable, slow, risky or new. The sandbox has no credit data, so
	// this is how a real sandbox payer gets a payment history.
	ExtraClients map[string]string
	AIModel      string // shown in /meta; empty means rules only
	Now          func() time.Time
}

// SeedClient is a synthetic payer history. The PayPal sandbox has no real credit
// data, so Advance ships with a few clearly labelled demo clients.
type SeedClient struct {
	Key     string                     `json:"key"`
	Email   string                     `json:"email"`
	Label   string                     `json:"label"`
	History underwriting.ClientHistory `json:"history"`
}

var seedClients = []SeedClient{
	{"reliable", "reliable@example.com", "Pays on time", underwriting.ClientHistory{Paid: 12, Late: 1, AvgDaysLate: 2}},
	{"slow", "slow@example.com", "Often pays late", underwriting.ClientHistory{Paid: 6, Late: 4, AvgDaysLate: 18}},
	{"risky", "risky@example.com", "Has defaulted", underwriting.ClientHistory{Paid: 2, Late: 1, Defaulted: 2, AvgDaysLate: 30}},
	{"new", "newclient@example.com", "No history yet", underwriting.ClientHistory{}},
}

type Server struct {
	pay      Payments
	led      *ledger.Ledger
	scorer   underwriting.Scorer
	reviewer ai.Reviewer
	opts     Options
	seeds    []SeedClient

	mu        sync.Mutex
	invoices  map[string]*Invoice
	histories map[string]underwriting.ClientHistory
	events    []string        // processed webhook event IDs, oldest first
	eventSet  map[string]bool // same, for lookup
	reserved  int64           // advances in flight, counted against the exposure cap
}

func New(pay Payments, led *ledger.Ledger, sc underwriting.Scorer, rv ai.Reviewer, opts Options) *Server {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{
		pay: pay, led: led, scorer: sc, reviewer: rv, opts: opts,
		invoices:  map[string]*Invoice{},
		histories: map[string]underwriting.ClientHistory{},
		eventSet:  map[string]bool{},
	}
	s.seeds = append(s.seeds, seedClients...)
	for _, c := range seedClients {
		s.histories[c.Email] = c.History
	}
	emails := make([]string, 0, len(opts.ExtraClients))
	for e := range opts.ExtraClients {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	for _, e := range emails {
		for _, base := range seedClients {
			if base.Key == opts.ExtraClients[e] {
				email := strings.ToLower(strings.TrimSpace(e))
				s.seeds = append(s.seeds, SeedClient{Key: base.Key, Email: email, Label: base.Label + " (your sandbox account)", History: base.History})
				s.histories[email] = base.History
			}
		}
	}
	return s
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"ok": "true"}) })
	mux.HandleFunc("GET /meta", s.meta)
	mux.HandleFunc("POST /invoices", s.createInvoice)
	mux.HandleFunc("GET /invoices", s.listInvoices)
	mux.HandleFunc("GET /invoices/{id}", s.getInvoice)
	mux.HandleFunc("POST /invoices/{id}/advance", s.advance)
	mux.HandleFunc("GET /ledger", s.ledgerView)
	mux.HandleFunc("POST /webhooks/paypal", s.webhook)
	if s.opts.DemoMode {
		mux.HandleFunc("POST /demo/invoices/{id}/pay", s.demoPay)
		mux.HandleFunc("POST /demo/invoices/{id}/default", s.demoDefault)
	}
	return s.wrap(mux)
}

// Mount lets main attach extra handlers (the dashboard) behind the same middleware.
func (s *Server) Mount(extra map[string]http.Handler) http.Handler {
	mux := http.NewServeMux()
	for p, h := range extra {
		mux.Handle(p, h)
	}
	mux.Handle("/", s.Routes())
	return mux
}

func (s *Server) meta(w http.ResponseWriter, _ *http.Request) {
	ai := "rules"
	if s.opts.AIModel != "" {
		ai = "claude"
	}
	writeJSON(w, 200, map[string]any{
		"demo_mode": s.opts.DemoMode, "underwriting": ai, "model": s.opts.AIModel,
		"auth_required": s.opts.APIKey != "", "clients": s.seeds,
	})
}

func validEmail(e string) bool {
	if len(e) > 254 {
		return false
	}
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FreelancerEmail string `json:"freelancer_email"`
		ClientEmail     string `json:"client_email"`
		Description     string `json:"description"`
		AmountCents     int64  `json:"amount_cents"`
		Currency        string `json:"currency"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, 400, "Request body must be valid JSON.")
		return
	}
	in.FreelancerEmail = strings.TrimSpace(in.FreelancerEmail)
	in.ClientEmail = strings.ToLower(strings.TrimSpace(in.ClientEmail))
	in.Description = strings.TrimSpace(in.Description)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Description == "" {
		in.Description = "Freelance services"
	}
	switch {
	case !validEmail(in.FreelancerEmail), !validEmail(in.ClientEmail):
		writeErr(w, 400, "Enter a valid freelancer email and client email.")
		return
	case in.AmountCents < 100 || in.AmountCents > 10_000_000:
		writeErr(w, 400, "Amount must be between 1.00 and 100,000.00.")
		return
	case len(in.Currency) != 3:
		writeErr(w, 400, "Currency must be a 3-letter code such as USD.")
		return
	case len(in.Description) > 200:
		writeErr(w, 400, "Description must be 200 characters or fewer.")
		return
	}

	ctx := r.Context()
	id, err := s.pay.CreateDraftInvoice(ctx, paypal.InvoiceInput{
		Currency: in.Currency, Description: in.Description,
		AmountCents: in.AmountCents, ClientEmail: in.ClientEmail,
	})
	if err != nil {
		log.Printf("create invoice: %v", err)
		writeErr(w, 502, "PayPal did not create the invoice. Check your sandbox credentials and try again.")
		return
	}
	if err := s.pay.SendInvoice(ctx, id); err != nil {
		log.Printf("send invoice %s: %v", id, err)
		writeErr(w, 502, "PayPal created the invoice but could not send it. Try again.")
		return
	}
	payURL, err := s.pay.PayerViewURL(ctx, id)
	if err != nil {
		log.Printf("payer url %s: %v", id, err) // not fatal: the invoice is already sent
	}

	inv := &Invoice{ID: id, FreelancerEmail: in.FreelancerEmail, ClientEmail: in.ClientEmail,
		Description: in.Description, AmountCents: in.AmountCents, Currency: in.Currency,
		Status: StatusSent, PayURL: payURL, CreatedAt: s.opts.Now()}
	s.mu.Lock()
	s.invoices[id] = inv
	cp := *inv
	s.persistLocked()
	s.mu.Unlock()
	writeJSON(w, 201, cp)
}

func (s *Server) listInvoices(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]Invoice, 0, len(s.invoices))
	for _, inv := range s.invoices {
		out = append(out, *inv)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	inv, ok := s.invoices[r.PathValue("id")]
	var cp Invoice
	if ok {
		cp = *inv
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, 404, "Invoice not found.")
		return
	}
	writeJSON(w, 200, cp)
}

// advance underwrites an invoice and, if approved, pays the freelancer.
func (s *Server) advance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	inv, ok := s.invoices[id]
	if !ok {
		s.mu.Unlock()
		writeErr(w, 404, "Invoice not found.")
		return
	}
	if inv.Status != StatusSent && inv.Status != StatusDeclined {
		st := inv.Status
		s.mu.Unlock()
		writeErr(w, 409, "This invoice is already "+st+".")
		return
	}
	prev := inv.Status
	inv.Status = StatusAdvancing // blocks concurrent advance requests
	snap := *inv
	hist := s.histories[snap.ClientEmail]
	s.mu.Unlock()

	// Money-moving calls must finish even if the browser disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()

	rule := s.scorer.Score(underwriting.Request{
		AmountCents: snap.AmountCents, FreelancerEmail: snap.FreelancerEmail,
		ClientEmail: snap.ClientEmail, History: hist,
	})
	res, err := s.reviewer.Review(ctx, ai.Input{
		AmountCents: snap.AmountCents, Currency: snap.Currency, Description: snap.Description,
		FreelancerEmail: snap.FreelancerEmail, ClientEmail: snap.ClientEmail, History: hist, Rule: rule,
	})
	if err != nil {
		log.Printf("review %s: %v", id, err)
		res, _ = ai.RulesReviewer{}.Review(ctx, ai.Input{History: hist, Rule: rule})
	}
	dec := ai.Apply(rule, res)

	// Reserve exposure under the lock so concurrent advances cannot exceed the cap.
	reserved := int64(0)
	if dec.Approved {
		total := dec.AdvanceCents + dec.FeeCents
		s.mu.Lock()
		if s.opts.MaxExposureCents > 0 && s.led.Balance(ledger.AdvancesReceivable)+s.reserved+total > s.opts.MaxExposureCents {
			dec.Approved, dec.AdvanceCents, dec.FeeCents = false, 0, 0
			dec.Reasons = append(dec.Reasons, "platform exposure limit reached")
		} else {
			s.reserved += total
			reserved = total
		}
		s.mu.Unlock()
	}
	release := func() {
		s.mu.Lock()
		s.reserved -= reserved
		s.mu.Unlock()
	}

	if !dec.Approved {
		s.mu.Lock()
		inv.Status, inv.Decision = StatusDeclined, &dec
		cp := *inv
		s.persistLocked()
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"decision": dec, "invoice": cp})
		return
	}

	batch, err := s.pay.CreatePayout(ctx, "adv-"+id, snap.FreelancerEmail, dec.AdvanceCents, snap.Currency, "Advance on invoice "+id)
	if err != nil {
		log.Printf("advance payout %s: %v", id, err)
		release()
		s.setStatus(id, prev)
		writeErr(w, 502, "PayPal could not send the payout. Nothing was charged; try again.")
		return
	}
	if _, err := s.led.Post(ledger.Disburse("disburse-"+id, dec.AdvanceCents, dec.FeeCents)); err != nil {
		log.Printf("ledger %s: %v", id, err)
	}
	now := s.opts.Now()
	s.mu.Lock()
	s.reserved -= reserved
	inv.Status, inv.Decision = StatusAdvanced, &dec
	inv.AdvanceCents, inv.FeeCents, inv.PayoutBatchID, inv.AdvancedAt = dec.AdvanceCents, dec.FeeCents, batch, &now
	cp := *inv
	s.persistLocked()
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"decision": dec, "invoice": cp})
}

type webhookEvent struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Resource  struct {
		ID      string `json:"id"`
		Invoice struct {
			ID string `json:"id"`
		} `json:"invoice"`
	} `json:"resource"`
}

func (e webhookEvent) invoiceID() string {
	if e.Resource.Invoice.ID != "" {
		return e.Resource.Invoice.ID
	}
	return e.Resource.ID
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "Bad request body.")
		return
	}
	ok, err := s.pay.VerifyWebhook(r.Context(), s.opts.WebhookID, r.Header, body)
	if err != nil || !ok {
		log.Printf("webhook verification failed: ok=%v err=%v", ok, err)
		writeErr(w, 401, "Invalid webhook signature.")
		return
	}
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeErr(w, 400, "Bad event.")
		return
	}
	if ev.ID != "" {
		s.mu.Lock()
		dup := s.eventSet[ev.ID]
		s.mu.Unlock()
		if dup {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	ctx := context.WithoutCancel(r.Context())
	var herr error
	switch ev.EventType {
	case "INVOICING.INVOICE.PAID":
		herr = s.settle(ctx, ev.invoiceID())
	case "INVOICING.INVOICE.CANCELLED":
		herr = s.cancelOrDefault(ev.invoiceID())
	}
	if herr != nil {
		log.Printf("webhook %s %s: %v", ev.EventType, ev.invoiceID(), herr)
		writeErr(w, 500, "Processing failed; PayPal will retry.")
		return
	}
	if ev.ID != "" {
		s.mu.Lock()
		s.eventSet[ev.ID] = true
		s.events = append(s.events, ev.ID)
		if len(s.events) > 1000 {
			delete(s.eventSet, s.events[0])
			s.events = s.events[1:]
		}
		s.persistLocked()
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

// settle repays the advance and pays the freelancer the remainder.
// Every step is idempotent, so webhook retries are safe.
func (s *Server) settle(ctx context.Context, id string) error {
	s.mu.Lock()
	inv, ok := s.invoices[id]
	if !ok || (inv.Status != StatusAdvanced && inv.Status != StatusSettling) {
		s.mu.Unlock()
		return nil // unknown, never advanced, or already closed: nothing for us to settle
	}
	inv.Status = StatusSettling
	amount, owed := inv.AmountCents, inv.AdvanceCents+inv.FeeCents
	freelancer, currency, client := inv.FreelancerEmail, inv.Currency, inv.ClientEmail
	s.mu.Unlock()

	if _, err := s.led.Post(ledger.Settle("settle-"+id, amount, owed)); err != nil {
		return err
	}
	if rem := amount - owed; rem > 0 {
		if _, err := s.pay.CreatePayout(ctx, "rem-"+id, freelancer, rem, currency, "Remainder of invoice "+id); err != nil {
			return err
		}
		if _, err := s.led.Post(ledger.PayRemainder("payout-"+id, rem)); err != nil {
			return err
		}
	}

	now := s.opts.Now()
	s.mu.Lock()
	if inv.Status != StatusSettled {
		inv.Status, inv.ClosedAt = StatusSettled, &now
		h := s.histories[client]
		h.Paid++
		s.histories[client] = h // the next advance for this client is priced with the new record
	}
	s.persistLocked()
	s.mu.Unlock()
	return nil
}

// cancelOrDefault handles an invoice that will not be paid. If we advanced
// money against it, the receivable is written off and the client's record worsens.
func (s *Server) cancelOrDefault(id string) error {
	s.mu.Lock()
	inv, ok := s.invoices[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	switch inv.Status {
	case StatusSent, StatusDeclined:
		now := s.opts.Now()
		inv.Status, inv.ClosedAt = StatusCancelled, &now
		s.persistLocked()
		s.mu.Unlock()
		return nil
	case StatusAdvanced:
		owed, client := inv.AdvanceCents+inv.FeeCents, inv.ClientEmail
		s.mu.Unlock()
		if _, err := s.led.Post(ledger.WriteOff("writeoff-"+id, owed)); err != nil {
			return err
		}
		now := s.opts.Now()
		s.mu.Lock()
		inv.Status, inv.ClosedAt = StatusDefaulted, &now
		h := s.histories[client]
		h.Defaulted++
		s.histories[client] = h
		s.persistLocked()
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return nil
}

// SweepDefaults writes off advances that have been unpaid for longer than
// DefaultAfter. It returns how many invoices were written off.
func (s *Server) SweepDefaults() int {
	if s.opts.DefaultAfter <= 0 {
		return 0
	}
	cutoff := s.opts.Now().Add(-s.opts.DefaultAfter)
	s.mu.Lock()
	var due []string
	for id, inv := range s.invoices {
		if inv.Status == StatusAdvanced && inv.AdvancedAt != nil && inv.AdvancedAt.Before(cutoff) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	n := 0
	for _, id := range due {
		if err := s.cancelOrDefault(id); err != nil {
			log.Printf("sweep %s: %v", id, err)
			continue
		}
		n++
	}
	return n
}

func (s *Server) ledgerView(w http.ResponseWriter, _ *http.Request) {
	bal := s.led.Balances()
	var sum int64
	for _, v := range bal {
		sum += v
	}
	entries := s.led.Entries()
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if len(entries) > 30 {
		entries = entries[:30]
	}
	writeJSON(w, 200, map[string]any{
		"balances": bal, "entries": entries,
		"exposure_cents": bal[ledger.AdvancesReceivable], "balanced": sum == 0,
	})
}

func (s *Server) demoPay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.settle(context.WithoutCancel(r.Context()), id); err != nil {
		log.Printf("demo pay %s: %v", id, err)
		writeErr(w, 502, "Settlement failed: "+err.Error())
		return
	}
	s.getInvoice(w, r)
}

func (s *Server) demoDefault(w http.ResponseWriter, r *http.Request) {
	if err := s.cancelOrDefault(r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.getInvoice(w, r)
}

func (s *Server) setStatus(id, status string) {
	s.mu.Lock()
	if inv, ok := s.invoices[id]; ok {
		inv.Status = status
	}
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
