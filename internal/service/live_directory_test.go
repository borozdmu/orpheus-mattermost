package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

func TestLiveDirectoryProjectsCurrentMembers(t *testing.T) {
	t.Setenv("ORPHEUS_LIVE_AUDIENCE", "true")
	first, entrant, bot := strings.Repeat("a", 26), strings.Repeat("c", 26), strings.Repeat("b", 26)
	binding := config.Access{Namespace: "mattermost:test", BotID: bot, ScopeContext: map[string]any{"agent_id": "finance"}, VerifyAccountCards: true, IdentityProjection: config.IdentityProjection{ActorField: "requester", AudienceField: "readers", Subjects: map[string]config.SubjectID{first: config.SubjectID("42")}}}
	accounts := map[string]int{first: 42, entrant: 55}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"request_id"`
			Method string `json:"method"`
			Params struct {
				Provider string   `json:"provider"`
				AgentKey string   `json:"agent_key"`
				IDs      []string `json:"external_ids"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "external.resolve_audience_accounts" || req.Params.Provider != "mattermost" || req.Params.AgentKey != "finance" || len(req.Params.IDs) != 2 {
			t.Error("unexpected directory request", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "request_id": req.ID, "result": accounts})
	}))
	defer srv.Close()
	orig := accountDirectoryHTTP
	accountDirectoryHTTP = srv.Client()
	t.Cleanup(func() { accountDirectoryHTTP = orig })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", srv.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("k", 32))
	roster := []string{first, bot, entrant}
	projected, _, err := projectAccess(binding, roster, entrant)
	if err != nil || string(projected["readers"]) != "[42,55]" {
		t.Fatal(projected, err)
	}
	if len(binding.IdentityProjection.Subjects) != 1 {
		t.Fatal("configured subjects changed")
	}
	projected, _, err = projectAccess(binding, []string{bot, first}, first)
	if err != nil || string(projected["readers"]) != "[42]" {
		t.Fatal(projected, err)
	}
	accounts[first] = 41
	if _, _, err = projectAccess(binding, roster, entrant); err == nil {
		t.Fatal("accepted changed account binding")
	}
	delete(accounts, entrant)
	if _, _, err = projectAccess(binding, roster, entrant); err == nil {
		t.Fatal("accepted incomplete audience")
	}
}
