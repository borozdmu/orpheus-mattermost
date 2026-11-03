package delivery

import "testing"

func TestCostBasisSurvivesReconciliation(t *testing.T) {
	p, _, part := setup()
	part.Receipt.CostFooter = "frozen estimate"
	part.Receipt.Attachments = nil
	if err := p.Publish(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	if got := p.FrozenCost("session", "run"); got != "frozen estimate" {
		t.Fatal(got)
	}
	if got := p.FrozenCost("session", "other"); got != "" {
		t.Fatal(got)
	}
}
