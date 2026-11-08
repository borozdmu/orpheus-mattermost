package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

func TestAccountCardCheckUsesTrustedMappingAndClosedDirectoryFailure(t *testing.T) {
	const peer = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	access := &config.Access{VerifyAccountCards: true, BotID: "bot", ScopeContext: map[string]any{"agent_id": "mark"},
		IdentityProjection: config.IdentityProjection{Subjects: map[string]config.SubjectID{peer: config.SubjectID("7")}}}
	var accepted atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"request_id"`
			Method string `json:"method"`
			Params struct {
				Accounts map[string]int `json:"accounts"`
				Provider string         `json:"provider"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "external.confirm_audience" || req.Params.Provider != "mattermost" || len(req.Params.Accounts) != 1 || req.Params.Accounts[peer] != 7 || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			http.Error(w, "invalid", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": accepted.Load(), "request_id": req.ID})
	}))
	defer srv.Close()
	original := accountDirectoryHTTP
	accountDirectoryHTTP = srv.Client()
	t.Cleanup(func() { accountDirectoryHTTP = original })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", srv.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("t", 32))
	roster := []string{"bot", peer}
	if confirmAccountCards(t.Context(), access, roster) == nil {
		t.Fatal("unconfirmed cards accepted")
	}
	accepted.Store(true)
	if err := confirmAccountCards(t.Context(), access, roster); err != nil {
		t.Fatal(err)
	}
	if confirmAccountCards(t.Context(), access, append(roster, "unknown")) == nil {
		t.Fatal("unknown reader accepted")
	}
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", "")
	if confirmAccountCards(t.Context(), access, roster) == nil {
		t.Fatal("directory failure accepted")
	}
}
