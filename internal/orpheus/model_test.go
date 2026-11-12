package orpheus

import (
	"strings"
	"testing"

	api "github.com/orpheus-agents/orpheus/client"
)

func TestModelCatalogIsScopedToWorkflow(t *testing.T) {
	items := []api.Profile{
		{Name: "mark", Model: new("alpha"), Codex: api.CodexProfile{Effort: new(api.CodexProfileEffortHigh)}},
		{Name: "mark--fast", Model: new("beta"), Codex: api.CodexProfile{Effort: new(api.CodexProfileEffortLow)}},
		{Name: "finance", Model: new("other")},
	}
	name, msg := SelectModel(items, "mark", "mark--fast", nil)
	if name != "" || !strings.Contains(msg, "Текущая настройка: beta low") || strings.Contains(msg, "other") {
		t.Fatal(name, msg)
	}
	name, _ = SelectModel(items, "mark", "mark", []string{"beta", "low"})
	if name != "mark--fast" {
		t.Fatal(name)
	}
	for _, args := range [][]string{{"beta", "high"}, {"other", "default"}, {"beta"}} {
		if name, _ := SelectModel(items, "mark", "mark", args); name != "" {
			t.Fatalf("accepted %q", args)
		}
	}
}
