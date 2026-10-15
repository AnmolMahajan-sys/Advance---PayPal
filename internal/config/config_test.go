package config

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultsAndPortFallback(t *testing.T) {
	c, err := load(env(map[string]string{"PAYPAL_CLIENT_ID": "a", "PAYPAL_CLIENT_SECRET": "b", "PAYPAL_WEBHOOK_ID": "w", "PORT": "10000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":10000" || c.MaxInvoiceCents != 500_000 || c.DefaultAfter.Hours() != 24*60 || c.DemoMode {
		t.Fatalf("%+v", c)
	}
}

func TestRequiredSettings(t *testing.T) {
	if _, err := load(env(nil)); err == nil {
		t.Fatal("missing credentials must fail")
	}
	if _, err := load(env(map[string]string{"PAYPAL_CLIENT_ID": "a", "PAYPAL_CLIENT_SECRET": "b"})); err == nil {
		t.Fatal("missing webhook id must fail outside demo mode")
	}
	if _, err := load(env(map[string]string{"PAYPAL_CLIENT_ID": "a", "PAYPAL_CLIENT_SECRET": "b", "DEMO_MODE": "true"})); err != nil {
		t.Fatalf("demo mode may omit the webhook id: %v", err)
	}
}

func TestBadNumbersRejected(t *testing.T) {
	_, err := load(env(map[string]string{"PAYPAL_CLIENT_ID": "a", "PAYPAL_CLIENT_SECRET": "b", "DEMO_MODE": "true", "MAX_INVOICE_CENTS": "lots"}))
	if err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestDemoClients(t *testing.T) {
	base := map[string]string{"PAYPAL_CLIENT_ID": "a", "PAYPAL_CLIENT_SECRET": "b", "DEMO_MODE": "true"}
	with := func(v string) map[string]string {
		m := map[string]string{"DEMO_CLIENTS": v}
		for k, x := range base {
			m[k] = x
		}
		return m
	}
	c, err := load(env(with(" Buyer@Sandbox.test = Reliable , other@x.test=slow ")))
	if err != nil || c.DemoClients["buyer@sandbox.test"] != "reliable" || c.DemoClients["other@x.test"] != "slow" {
		t.Fatalf("%+v %v", c.DemoClients, err)
	}
	for _, bad := range []string{"nope", "a@b.c=genius", "=reliable"} {
		if _, err := load(env(with(bad))); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
