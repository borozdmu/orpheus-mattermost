package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

func registeredBotUsers() map[string]mattermost.User {
	return map[string]mattermost.User{
		registeredBotOne: {ID: registeredBotOne, IsBot: true},
		registeredBotTwo: {ID: registeredBotTwo, IsBot: true},
	}
}

// Human account confirmation cannot attest to a bot's current registration.
// A separate trusted recipient decision must remain fresh through publication.
func TestRegisteredBotPublicationRechecksTrustedRegistryWithoutConfigChange(t *testing.T) {
	for _, change := range []string{"bot revoked", "owner changed", "owner grant revoked"} {
		t.Run(change, func(t *testing.T) {
			var active, grant atomic.Bool
			var owner, checks atomic.Int32
			active.Store(true)
			grant.Store(true)
			owner.Store(99)
			server := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     string          `json:"request_id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					http.Error(rw, "invalid", http.StatusBadRequest)
					return
				}
				allowed := true
				if request.Method == "external.confirm_bot_audience" {
					checks.Add(1)
					var params struct {
						Provider string `json:"provider"`
						Agent    string `json:"agent_key"`
						Bots     map[string]struct {
							ID    int `json:"bot_id"`
							Owner int `json:"owner_user_id"`
						} `json:"bots"`
					}
					if json.Unmarshal(request.Params, &params) != nil || params.Provider != "mattermost" || params.Agent != "mark" || len(params.Bots) != 2 || params.Bots[registeredBotOne].ID != 71 || params.Bots[registeredBotTwo].ID != 72 || params.Bots[registeredBotOne].Owner != 99 || params.Bots[registeredBotTwo].Owner != 99 {
						t.Error("recipient proof did not bind the exact source, registration and owner")
						http.Error(rw, "invalid", http.StatusBadRequest)
						return
					}
					allowed = active.Load() && grant.Load() && owner.Load() == 99
				} else if request.Method != "external.confirm_audience" {
					http.Error(rw, "unsupported method", http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(rw).Encode(map[string]any{"request_id": request.ID, "ok": true, "result": allowed})
			}))
			defer server.Close()
			original := accountDirectoryHTTP
			accountDirectoryHTTP = server.Client()
			t.Cleanup(func() { accountDirectoryHTTP = original })
			t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", server.URL)
			t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
			t.Setenv("ORPHEUS_AUDIENCE_SECRET", strings.Repeat("a", 32))

			e, api, mm, _, key, w := protectedFixture(t)
			e.SourceID = key.Source
			w.AgentID = "mark"
			w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
			mm.audience = append(mm.audience, registeredBotOne, registeredBotTwo)
			mm.users = registeredBotUsers()
			root := corporateStateBinding(w, key.Channel)
			e.Config.Workflows[0] = root
			var err error
			w, err = root.ForChat(key.Channel)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Thread(t.Context(), w, key, mm.channel, true); err != nil || len(api.attempts) != 1 {
				t.Fatal("initial valid audience was not admitted", err)
			}
			if checks.Load() == 0 {
				t.Fatal("admission did not confirm the registered bot audience")
			}
			const privateAnswer = "PRIVATE_BOT_PROTECTED_OUTPUT"
			session := &api.snapshot.Sessions[0]
			run := &session.Runs[0]
			run.Status, run.FinalMessageID = "completed", "final"
			session.Messages = append(session.Messages, conversation.Message{ID: "final", RunID: run.ID, Role: "assistant", Kind: "answer", Position: 1, Text: privateAnswer})
			before := checks.Load()
			switch change {
			case "bot revoked":
				active.Store(false)
			case "owner changed":
				owner.Store(100)
			case "owner grant revoked":
				grant.Store(false)
			}
			// Neither connector configuration nor the source membership changed.
			_ = e.Thread(t.Context(), w, key, mm.channel, true)
			for _, post := range mm.posts {
				if strings.Contains(post.Message, privateAnswer) {
					t.Fatal("protected answer published after trusted bot registration changed")
				}
			}
			if checks.Load() <= before {
				t.Fatal("publication did not obtain a fresh trusted bot decision")
			}
			active.Store(true)
			grant.Store(true)
			owner.Store(99)
			for range 2 {
				if err = e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
					t.Fatal(err)
				}
			}
			published := 0
			for _, post := range mm.posts {
				if strings.Contains(post.Message, privateAnswer) {
					published++
				}
			}
			if published != 1 {
				t.Fatalf("restored bot authority must publish once, got %d", published)
			}
		})
	}
}

func TestCorporateBotAdmissionRequiresProviderCapability(t *testing.T) {
	var proofs atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"request_id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(rw, "invalid", 400)
			return
		}
		if req.Method == "external.confirm_bot_audience" {
			proofs.Add(1)
			http.Error(rw, "unsupported", 404)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"request_id": req.ID, "ok": true, "result": true})
	}))
	defer server.Close()
	previous := accountDirectoryHTTP
	accountDirectoryHTTP = server.Client()
	t.Cleanup(func() { accountDirectoryHTTP = previous })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", server.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
	t.Setenv("ORPHEUS_AUDIENCE_SECRET", strings.Repeat("a", 32))
	e, api, mm, _, key, w := protectedFixture(t)
	w.AgentID = "mark"
	w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
	mm.audience = append(mm.audience, registeredBotOne, registeredBotTwo)
	mm.users = registeredBotUsers()
	_ = e.Thread(t.Context(), w, key, mm.channel, true)
	if len(api.attempts) != 0 || proofs.Load() != 1 {
		t.Fatal("unsupported recipient proof reached model or fell back to human-only cards")
	}
}

func TestCorporatePublicTeamKeepsOutsideHumanAndMultipleBots(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "unmapped outside human"}[confirmed], func(t *testing.T) {
			corporateDirectory(t)
			e, api, mm, _, key, w := protectedFixture(t)
			e.SourceID = key.Source
			w.AgentID, w.PublicTeamAudience = "mark", true
			w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
			const outside = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			if confirmed {
				w.Access.IdentityProjection.Subjects[outside] = config.SubjectID(`77`)
			}
			mm.channel.Type, mm.channel.TeamID, mm.channel.Shared = "O", strings.Repeat("t", 26), new(false)
			mm.users = registeredBotUsers()
			source := &publicAudienceSource{accessSource: mm, team: []string{mm.bot, mm.posts[0].UserID, outside, registeredBotOne, registeredBotTwo}, revision: "full-team-1"}
			slices.Sort(source.team)
			e.MM = source
			e.Config.Workflows[0] = w
			err := e.Thread(t.Context(), w, key, mm.channel, true)
			if !confirmed {
				if len(api.attempts) != 0 {
					t.Fatal("outside reader was silently replaced by channel members")
				}
				return
			}
			if err != nil || len(api.attempts) != 1 {
				t.Fatal("confirmed public audience not admitted", err)
			}
			task, err := agentcontract.Decode(*api.attempts[0].Envelope.AccessContext.Assignment)
			if err != nil {
				t.Fatal(err)
			}
			if task.Audience.Kind != "team-current" || len(task.Readers) != 4 || len(mm.audience) != 2 {
				t.Fatal("public audience was reduced to channel members", task.Readers)
			}
			var ids []int
			if json.Unmarshal(api.attempts[0].Envelope.AccessContext.ScopeContext["reader_user_ids"], &ids) != nil || !slices.Equal(ids, []int{42, 77, 99}) {
				t.Fatal("common public scope lost a human or bot owner", ids)
			}
			bots := 0
			for _, reader := range task.Readers {
				if reader.BotID != "" {
					bots++
					if reader.PrincipalID != "99" {
						t.Fatal("shared bot owner lost")
					}
				}
			}
			if bots != 2 {
				t.Fatal("two registered bot principals not retained")
			}
		})
	}
}
