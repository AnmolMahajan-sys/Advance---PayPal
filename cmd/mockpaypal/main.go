// Command mockpaypal runs the offline PayPal stand-in used for local demos.
package main

import (
	"log"
	"net/http"
	"os"

	"advance/internal/mockpaypal"
)

func main() {
	addr := env("ADDR", ":9090")
	target := env("WEBHOOK_TARGET", "http://localhost:8080/webhooks/paypal")
	log.Printf("mock PayPal on %s, webhooks -> %s", addr, target)
	log.Fatal(http.ListenAndServe(addr, mockpaypal.New(target).Handler()))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
