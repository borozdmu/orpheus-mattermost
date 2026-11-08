package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCorporateMigratedCatalog(t *testing.T) {
	root := os.Getenv("CORPORATE_MIGRATION_DIR")
	if root == "" {
		t.Skip("manifest runner supplies captured settings migration")
	}
	values := map[string]string{"WORKFLOWS_DIR": filepath.Join(root, "connectors/mattermost"), "ORPHEUS_BASE_URL": "http://orpheus.invalid", "TRANSCRIPTION_URL": "https://speech.invalid/v1/audio/transcriptions", "TRANSCRIPTION_MODEL": "test"}
	cfg, err := load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]bool{}
	for _, w := range cfg.Workflows {
		if w.AgentID == "" || len(w.Instructions) == 0 || agents[w.AgentID] {
			t.Fatal("missing or duplicate agent role")
		}
		agents[w.AgentID] = true
		if w.AgentID == "counsel-beta" && w.LocalBeforeRunPython == "" {
			t.Fatal("Beta's masking input processor lost")
		}
	}
	if len(agents) != 6 || !agents["counsel"] || !agents["counsel-beta"] || !agents["finance"] || !agents["devkord"] || !agents["cxassist"] || !agents["mark"] {
		t.Fatal("incomplete migration")
	}
}
