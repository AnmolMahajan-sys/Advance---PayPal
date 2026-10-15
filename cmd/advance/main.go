package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"advance/internal/ai"
	"advance/internal/api"
	"advance/internal/config"
	"advance/internal/ledger"
	"advance/internal/paypal"
	"advance/internal/underwriting"
	"advance/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	pp := paypal.New(cfg.PayPalBaseURL, cfg.ClientID, cfg.Secret)
	reviewer := ai.New(cfg.AnthropicKey, cfg.AnthropicModel, cfg.AnthropicBaseURL)

	opts := api.Options{
		WebhookID: cfg.WebhookID, DemoMode: cfg.DemoMode, APIKey: cfg.APIKey,
		MaxExposureCents: cfg.MaxExposureCents, DefaultAfter: cfg.DefaultAfter, DataFile: cfg.DataFile,
		ExtraClients: cfg.DemoClients,
	}
	if cfg.AnthropicKey != "" {
		opts.AIModel = cfg.AnthropicModel
	}
	srv := api.New(pp, ledger.New(), underwriting.RuleScorer{MaxAdvanceCents: cfg.MaxInvoiceCents}, reviewer, opts)
	if err := srv.LoadState(); err != nil {
		log.Fatalf("load state: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() { // write off advances that have gone unpaid for too long
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := srv.SweepDefaults(); n > 0 {
					log.Printf("wrote off %d overdue advance(s)", n)
				}
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Mount(map[string]http.Handler{"GET /{$}": web.Handler()}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second, // payouts and AI review can be slow
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	mode := "rules"
	if cfg.AnthropicKey != "" {
		mode = "claude (" + cfg.AnthropicModel + ")"
	}
	log.Printf("advance listening on %s | underwriting: %s | demo mode: %v | paypal: %s", cfg.Addr, mode, cfg.DemoMode, cfg.PayPalBaseURL)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
