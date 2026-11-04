package feedback

import (
	"strings"
	"testing"
	"time"
)

func TestMessageCost(t *testing.T) {
	d := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	b := Basis{FX: map[string]float64{"2026-11-03": 80}, Models: map[string]Price{"sol": {Input: 2, Cached: 0.1, Output: 10}}}
	// Reasoning is already part of output; cached input replaces normal input.
	got := Format("sol", Usage{Input: 1000000, Cached: 500000, Output: 100000, Total: 1100000}, d, b)
	if !strings.Contains(got, "≈164,00 ₽") {
		t.Fatal(got)
	}
	for _, u := range []Usage{{}, {Input: 1, Cached: 2, Total: 1}, {Input: -1, Total: 1}} {
		if !strings.Contains(Format("sol", u, d, b), "недоступна") {
			t.Fatal(u)
		}
	}
	if !strings.Contains(Format("missing", Usage{Total: 1}, d, b), "недоступна") {
		t.Fatal("unknown price")
	}
	if !strings.Contains(Format("sol", Usage{Total: 1}, d.AddDate(0, 0, 8), b), "недоступна") {
		t.Fatal("stale FX")
	}
	// Later exchange rates cannot rewrite an existing run's delivery bytes.
	before := Format("sol", Usage{Input: 1, Total: 1}, d, b)
	b.FX["2026-11-04"] = 90
	if before != Format("sol", Usage{Input: 1, Total: 1}, d, b) {
		t.Fatal("unstable receipt")
	}
}

func TestCutover(t *testing.T) {
	t.Setenv("BOT_FEEDBACK_SINCE", "1793667600")
	if Enabled(time.Unix(1793667599, 0)) || !Enabled(time.Unix(1793667600, 0)) {
		t.Fatal("cutover")
	}
}
