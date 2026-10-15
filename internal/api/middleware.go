package api

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wrap applies, outermost first: panic recovery, security headers, request
// logging, API-key auth and per-IP rate limiting on the JSON API.
func (s *Server) wrap(next http.Handler) http.Handler {
	rl := &limiter{rate: 10, burst: 40, m: map[string]*bucket{}}
	return recoverer(headers(logger(func(w http.ResponseWriter, r *http.Request) {
		if isAPI(r.URL.Path) {
			if !rl.allow(clientIP(r), time.Now()) {
				w.Header().Set("Retry-After", "1")
				writeErr(w, http.StatusTooManyRequests, "Too many requests. Slow down and retry.")
				return
			}
			if s.opts.APIKey != "" && !keyOK(r, s.opts.APIKey) {
				writeErr(w, http.StatusUnauthorized, "A valid API key is required.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})))
}

// The webhook is authenticated by PayPal's signature, not the API key.
func isAPI(p string) bool {
	return strings.HasPrefix(p, "/invoices") || strings.HasPrefix(p, "/ledger") || strings.HasPrefix(p, "/demo/")
}

func keyOK(r *http.Request, key string) bool {
	got := r.Header.Get("X-API-Key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	m     map[string]*bucket
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) > 10_000 { // bound memory under a spray of addresses
			l.m = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func logger(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next(sw, r)
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic: %v", v)
				writeErr(w, http.StatusInternalServerError, "Something went wrong on our side.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
