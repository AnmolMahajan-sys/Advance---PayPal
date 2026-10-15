// Package underwriting prices an invoice advance with deterministic, explainable
// rules. The AI layer (package ai) can only tighten what these rules approve.
package underwriting

import (
	"fmt"
	"strings"
)

type ClientHistory struct {
	Paid        int     `json:"paid"`
	Late        int     `json:"late"`
	Defaulted   int     `json:"defaulted"`
	AvgDaysLate float64 `json:"avg_days_late"`
}

type Request struct {
	AmountCents     int64
	FreelancerEmail string
	ClientEmail     string
	History         ClientHistory
}

type Decision struct {
	Approved     bool     `json:"approved"`
	RiskScore    float64  `json:"risk_score"`
	AdvanceCents int64    `json:"advance_cents"`
	FeeCents     int64    `json:"fee_cents"`
	Reasons      []string `json:"reasons"`

	// Filled in by the AI review step.
	Explanation string   `json:"explanation,omitempty"`
	RedFlags    []string `json:"red_flags,omitempty"`
	ReviewedBy  string   `json:"reviewed_by,omitempty"` // "claude" or "rules"
	Verdict     string   `json:"ai_verdict,omitempty"`  // approve | reduce | decline
}

type Scorer interface {
	Score(Request) Decision
}

type RuleScorer struct {
	MaxAdvanceCents int64 // largest invoice we will advance against
}

func (r RuleScorer) Score(q Request) Decision {
	h := q.History
	total := h.Paid + h.Late + h.Defaulted
	var reasons []string

	risk := 0.05
	if total == 0 {
		risk = 0.60
		reasons = append(reasons, "no payment history for this client")
	} else {
		lateRate := float64(h.Late) / float64(total)
		defRate := float64(h.Defaulted) / float64(total)
		risk += 0.5*lateRate + 2.0*defRate + 0.01*h.AvgDaysLate
		if total < 3 {
			risk += 0.15
			reasons = append(reasons, "thin payment history")
		}
		reasons = append(reasons, fmt.Sprintf("%d paid, %d late, %d defaulted", h.Paid, h.Late, h.Defaulted))
	}
	if risk > 1 {
		risk = 1
	}

	d := Decision{RiskScore: risk, Reasons: reasons}
	switch {
	case q.AmountCents <= 0:
		d.Reasons = append(d.Reasons, "invalid amount")
	case strings.EqualFold(strings.TrimSpace(q.FreelancerEmail), strings.TrimSpace(q.ClientEmail)):
		d.RiskScore = 1
		d.Reasons = append(d.Reasons, "freelancer and client are the same party")
	case r.MaxAdvanceCents > 0 && q.AmountCents > r.MaxAdvanceCents:
		d.Reasons = append(d.Reasons, "invoice exceeds the per-invoice limit")
	case total == 0:
		d.Reasons = append(d.Reasons, "new clients are not advanced until they have a payment history")
	case h.Defaulted >= 2 || risk > 0.60:
		d.Reasons = append(d.Reasons, "client risk is too high")
	default:
		rate := 0.90 - 0.30*risk
		feeBps := 150 + 600*risk
		d.Approved = true
		d.AdvanceCents = int64(float64(q.AmountCents) * rate)
		d.FeeCents = int64(float64(q.AmountCents) * feeBps / 10000)
		d.Reasons = append(d.Reasons, fmt.Sprintf("advance rate %.0f%%, fee %.2f%%", rate*100, feeBps/100))
	}
	return d
}
