package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr          string
	PayPalBaseURL string
	ClientID      string
	Secret        string
	WebhookID     string

	AnthropicKey     string
	AnthropicModel   string
	AnthropicBaseURL string

	DemoMode         bool              // exposes /demo/* endpoints that simulate the client paying
	APIKey           string            // optional shared key for the JSON API
	DataFile         string            // optional JSON snapshot so state survives restarts
	MaxInvoiceCents  int64             // per-invoice advance limit
	MaxExposureCents int64             // total outstanding advances we allow
	DefaultAfter     time.Duration     // write off advances unpaid after this long
	DemoClients      map[string]string // email -> reliable|slow|risky|new, for real sandbox payer accounts
}

func Load() (Config, error) { return load(os.Getenv) }

func load(get func(string) string) (Config, error) {
	env := func(k, def string) string {
		if v := get(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:             env("ADDR", ":"+env("PORT", "8080")), // Render sets PORT
		PayPalBaseURL:    env("PAYPAL_BASE_URL", "https://api-m.sandbox.paypal.com"),
		ClientID:         get("PAYPAL_CLIENT_ID"),
		Secret:           get("PAYPAL_CLIENT_SECRET"),
		WebhookID:        get("PAYPAL_WEBHOOK_ID"),
		AnthropicKey:     get("ANTHROPIC_API_KEY"),
		AnthropicModel:   env("ANTHROPIC_MODEL", "claude-sonnet-5-5"),
		AnthropicBaseURL: get("ANTHROPIC_BASE_URL"),
		DemoMode:         strings.EqualFold(get("DEMO_MODE"), "true"),
		APIKey:           get("API_KEY"),
		DataFile:         get("DATA_FILE"),
	}
	var err error
	if c.MaxInvoiceCents, err = intEnv(get, "MAX_INVOICE_CENTS", 500_000); err != nil {
		return c, err
	}
	if c.MaxExposureCents, err = intEnv(get, "MAX_EXPOSURE_CENTS", 2_000_000); err != nil {
		return c, err
	}
	days, err := intEnv(get, "DEFAULT_AFTER_DAYS", 60)
	if err != nil {
		return c, err
	}
	c.DefaultAfter = time.Duration(days) * 24 * time.Hour

	if c.DemoClients, err = parseDemoClients(get("DEMO_CLIENTS")); err != nil {
		return c, err
	}

	if c.ClientID == "" || c.Secret == "" {
		return c, fmt.Errorf("PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET are required (see .env.example)")
	}
	if !c.DemoMode && c.WebhookID == "" {
		return c, fmt.Errorf("PAYPAL_WEBHOOK_ID is required unless DEMO_MODE=true (webhooks are signature-verified)")
	}
	return c, nil
}

func intEnv(get func(string) string, key string, def int64) (int64, error) {
	v := get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, v)
	}
	return n, nil
}

// parseDemoClients parses "buyer@x.com=reliable,other@y.com=slow".
func parseDemoClients(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		email, profile, ok := strings.Cut(part, "=")
		email, profile = strings.ToLower(strings.TrimSpace(email)), strings.ToLower(strings.TrimSpace(profile))
		switch {
		case !ok || email == "" || !strings.Contains(email, "@"):
			return nil, fmt.Errorf("DEMO_CLIENTS entry %q must look like email=profile", part)
		case profile != "reliable" && profile != "slow" && profile != "risky" && profile != "new":
			return nil, fmt.Errorf("DEMO_CLIENTS profile %q must be reliable, slow, risky or new", profile)
		}
		out[email] = profile
	}
	return out, nil
}
