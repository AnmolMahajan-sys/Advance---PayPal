package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"advance/internal/underwriting"
)

func approvedRule() underwriting.Decision {
	return underwriting.Decision{Approved: true, AdvanceCents: 80_000, FeeCents: 3_000, Reasons: []string{"ok"}}
}

func TestApplyNeverIncreases(t *testing.T) {
	cases := []struct {
		name string
		r    Result
		want int64
		ok   bool
	}{
		{"approve keeps", Result{Verdict: VerdictApprove}, 80_000, true},
		{"reduce shrinks", Result{Verdict: VerdictReduce, Multiplier: 0.5}, 40_000, true},
		{"multiplier above 1 is clamped", Result{Verdict: VerdictReduce, Multiplier: 5}, 80_000, true},
		{"negative multiplier means zero", Result{Verdict: VerdictReduce, Multiplier: -1}, 0, false},
		{"decline stops", Result{Verdict: VerdictDecline}, 0, false},
	}
	for _, c := range cases {
		got := Apply(approvedRule(), c.r)
		if got.AdvanceCents != c.want || got.Approved != c.ok {
			t.Errorf("%s: advance=%d approved=%v", c.name, got.AdvanceCents, got.Approved)
		}
		if got.AdvanceCents > 80_000 {
			t.Errorf("%s: advance increased", c.name)
		}
	}
}

func TestApplyCannotApproveADeclinedRule(t *testing.T) {
	d := Apply(underwriting.Decision{Approved: false}, Result{Verdict: VerdictApprove, Multiplier: 1})
	if d.Approved || d.AdvanceCents != 0 {
		t.Fatalf("AI must not override a rule decline: %+v", d)
	}
}

func fakeAnthropic(t *testing.T, status int, content any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tc, _ := body["tool_choice"].(map[string]any); tc["name"] != "submit_review" {
			t.Errorf("tool call must be forced: %v", body["tool_choice"])
		}
		if !strings.Contains(body["system"].(string), "untrusted") {
			t.Error("system prompt must warn about untrusted text")
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"content": content})
	}))
}

func TestClaudeParsesToolCall(t *testing.T) {
	srv := fakeAnthropic(t, 200, []any{map[string]any{"type": "tool_use", "name": "submit_review",
		"input": map[string]any{"verdict": "reduce", "advance_multiplier": 0.6, "explanation": "Large for this client.", "red_flags": []string{"big jump"}}}})
	defer srv.Close()
	r, err := (&Claude{APIKey: "k", Model: "m", BaseURL: srv.URL}).Review(context.Background(), Input{Description: "Logo design"})
	if err != nil || r.Verdict != VerdictReduce || r.Multiplier != 0.6 || r.Source != "claude" || len(r.RedFlags) != 1 {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestClaudeRejectsBadVerdictAndMissingTool(t *testing.T) {
	bad := fakeAnthropic(t, 200, []any{map[string]any{"type": "tool_use", "name": "submit_review",
		"input": map[string]any{"verdict": "pay_double", "explanation": "x"}}})
	defer bad.Close()
	if _, err := (&Claude{APIKey: "k", BaseURL: bad.URL}).Review(context.Background(), Input{}); err == nil {
		t.Fatal("unknown verdict must be an error")
	}
	none := fakeAnthropic(t, 200, []any{map[string]any{"type": "text", "text": "sure!"}})
	defer none.Close()
	if _, err := (&Claude{APIKey: "k", BaseURL: none.URL}).Review(context.Background(), Input{}); err == nil {
		t.Fatal("missing tool call must be an error")
	}
}

func TestChainFallsBackToRules(t *testing.T) {
	srv := fakeAnthropic(t, 500, []any{})
	defer srv.Close()
	rv := New("k", "m", srv.URL)
	r, err := rv.Review(context.Background(), Input{Rule: approvedRule()})
	if err != nil || r.Source != "rules" || !strings.Contains(r.Explanation, "unavailable") {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestNoKeyUsesRules(t *testing.T) {
	if _, ok := New("", "m", "").(RulesReviewer); !ok {
		t.Fatal("expected RulesReviewer without an API key")
	}
}

func TestRulesReviewerFlagsSharedDomain(t *testing.T) {
	r, _ := RulesReviewer{}.Review(context.Background(), Input{FreelancerEmail: "a@acme.com", ClientEmail: "b@ACME.com"})
	if len(r.RedFlags) == 0 {
		t.Fatal("expected a shared-domain flag")
	}
}
