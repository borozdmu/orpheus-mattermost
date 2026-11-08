package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus/agentcontract"
	api "github.com/orpheus-agents/orpheus/client"
)

func corporateDirectory(t *testing.T) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"request_id": in["request_id"], "ok": true, "result": true})
	}))
	original := accountDirectoryHTTP
	accountDirectoryHTTP = server.Client()
	t.Cleanup(func() { accountDirectoryHTTP = original; server.Close() })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", server.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
	t.Setenv("ORPHEUS_AUDIENCE_SECRET", strings.Repeat("a", 32))
}
func TestCorporateAdmissionReconfirmationAndAuthorIsolation(t *testing.T) {
	corporateDirectory(t)
	e, a, mm, _, key, w := protectedFixture(t)
	e.SourceID = key.Source
	w.PrivateChannels = true
	w.AgentID = "mark"
	w.Access.VerifyAccountCards = true
	w.Access.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	w.Access.IdentityProjection.ActorField = "actor_user_id"
	w.Access.IdentityProjection.AudienceField = "reader_user_ids"
	root := corporateStateBinding(w, key.Channel)
	e.Config.Workflows[0] = root
	var resolveErr error
	w, resolveErr = root.ForChat(key.Channel)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || a.attempts[0].Envelope.AccessContext.Assignment == nil {
		t.Fatal("missing corporate submission")
	}
	access := a.attempts[0].Envelope.AccessContext
	task, err := agentcontract.Decode(*access.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	if task.Agent != "mark" || task.Author.PrincipalID != "42" || task.Audience.Kind != "exact" {
		t.Fatal("wrong common assignment")
	}
	if err = e.confirmCorporate(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("CORPORATE_FIXTURES_DIR"); dir != "" {
		raw, _ := json.Marshal(api.CreateSession{AccessContext: access, AllowMultipleRuns: new(true), Configuration: api.ConfigurationInput{Agent: api.AgentInput{Profile: w.Profile, Instructions: new(w.Instructions)}, Sandbox: api.SandboxInput{Template: w.SandboxTemplate}, Limits: &api.LimitsInput{RunTimeoutSeconds: &w.RunTimeoutSeconds, MaxSessionTokens: &w.MaxSessionTokens}}, Messages: []api.TextMessage{{Text: task.Text}}})
		if err = os.WriteFile(filepath.Join(dir, "mattermost-request.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	reader := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	mm.audience = append(mm.audience, reader)
	if e.confirmCorporate(t.Context(), task) == nil {
		t.Fatal("changed membership confirmed")
	}
	appendAccessPost(mm, key, "xxxxxxxxxxxxxxxxxxxxxxxxxx", reader, "@bot write the spreadsheet", mm.posts[0].CreateAt+1)
	if err = e.Thread(t.Context(), w, key, mm.channel, false); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 {
		t.Fatal("another author clarified using original author's grant")
	}
	mm.audience = mm.audience[:len(mm.audience)-1]
	changed := e.Config
	changed.Workflows = nil
	e.setCorporateConfig(changed)
	if e.confirmCorporate(t.Context(), task) == nil {
		t.Fatal("removed binding confirmed after reload")
	}
}
func TestCorporateAudienceTypesFailClosed(t *testing.T) {
	corporateDirectory(t)
	for _, kind := range []string{"O", "G", "X"} {
		e, _, mm, _, key, w := protectedFixture(t)
		w.AgentID = "mark"
		mm.channel.Type = kind
		if _, _, err := e.confirmAudience(t.Context(), w, key); err == nil {
			t.Fatalf("unproven %s audience accepted", kind)
		}
	}
	e, _, mm, _, key, w := protectedFixture(t)
	w.AgentID = "mark"
	mm.channel.Shared = new(true)
	if _, _, err := e.confirmAudience(t.Context(), w, key); err == nil {
		t.Fatal("shared channel without complete remote coverage accepted")
	}
}

type publicAudienceSource struct {
	*accessSource
	team     []string
	revision string
}

func (s *publicAudienceSource) TeamAudience(context.Context, string) ([]string, string, error) {
	return slices.Clone(s.team), s.revision, nil
}
func TestCorporatePublicAdmissionUsesAllCurrentTeamUsers(t *testing.T) {
	corporateDirectory(t)
	e, a, base, _, key, w := protectedFixture(t)
	e.SourceID = key.Source
	key.Channel = "pppppppppppppppppppppppppp"
	base.channel.ID = key.Channel
	base.posts[0].ChannelID = key.Channel
	w.IncludeIDs = []string{key.Channel}
	w.AgentID = "mark"
	w.PublicTeamAudience = true
	w.Access.VerifyAccountCards = true
	w.Access.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	w.Access.IdentityProjection.ActorField = "actor_user_id"
	w.Access.IdentityProjection.AudienceField = "reader_user_ids"
	base.channel.Type = "O"
	base.channel.TeamID = "tttttttttttttttttttttttttt"
	base.channel.Shared = new(false)
	source := &publicAudienceSource{accessSource: base, team: []string{base.bot, base.posts[0].UserID, "nnnnnnnnnnnnnnnnnnnnnnnnnn"}, revision: "members-roles-1"}
	slices.Sort(source.team)
	e.MM = source
	e.Config.Workflows[0] = w
	if err := e.Thread(t.Context(), w, key, base.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 {
		t.Fatal("public team admission missing")
	}
	access := a.attempts[0].Envelope.AccessContext
	task, err := agentcontract.Decode(*access.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Readers) != 2 || task.Audience.Kind != "team-current" || len(base.audience) != 2 {
		t.Fatal("public channel members replaced full team")
	}
	if err = e.confirmCorporate(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("CORPORATE_FIXTURES_DIR"); dir != "" {
		raw, _ := json.Marshal(api.CreateSession{AccessContext: access, AllowMultipleRuns: new(true), Configuration: api.ConfigurationInput{Agent: api.AgentInput{Profile: w.Profile, Instructions: new(w.Instructions)}, Sandbox: api.SandboxInput{Template: w.SandboxTemplate}, Limits: &api.LimitsInput{RunTimeoutSeconds: &w.RunTimeoutSeconds, MaxSessionTokens: &w.MaxSessionTokens}}, Messages: []api.TextMessage{{Text: task.Text}}})
		if err = os.WriteFile(filepath.Join(dir, "mattermost-public-request.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source.revision = "permissions-2"
	if e.confirmCorporate(t.Context(), task) == nil {
		t.Fatal("permission/membership revision change accepted")
	}
	source.revision = "members-roles-1"
	source.team = append(source.team, "uuuuuuuuuuuuuuuuuuuuuuuuuu")
	if e.confirmCorporate(t.Context(), task) == nil {
		t.Fatal("unknown team reader accepted")
	}
}

func TestCorporatePeerRequiresRegisteredOwnerAndKeepsBothIdentities(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	peer := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	w.Access.Peer = &config.PeerRecipient{ID: peer, Field: "peer_id"}
	delete(w.Access.IdentityProjection.Subjects, peer)
	roster := []string{mm.bot, mm.posts[0].UserID, peer}
	if _, err := corporateReaders(w, roster); err == nil {
		t.Fatal("peer without employee owner accepted")
	}
	w.Access.Peer.BotID = config.SubjectID(`71`)
	w.Access.Peer.OwnerUserID = config.SubjectID(`77`)
	readers, err := corporateReaders(w, roster)
	if err != nil {
		t.Fatal(err)
	}
	if readers[len(readers)-1].ID != peer || readers[len(readers)-1].BotID != "71" || readers[len(readers)-1].PrincipalID != "77" {
		t.Fatal("bot registration lost")
	}
}
