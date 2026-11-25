package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus/agentcontract"
	"gopkg.in/yaml.v3"
)

const registeredBotOne = "pppppppppppppppppppppppppp"
const registeredBotTwo = "qqqqqqqqqqqqqqqqqqqqqqqqqq"

func registeredBotAccess(t *testing.T, bot, actor string) *config.Access {
	t.Helper()
	var access config.Access
	// Exercise the operator-facing configuration and its typed identities.
	source := fmt.Sprintf(`verify_account_cards: true
namespace: mattermost:test
bot_id: %s
scope_context: {agent_id: mark, policy_space_id: mark-test}
identity_projection:
  actor_field: actor_user_id
  audience_field: reader_user_ids
  bot_owners_field: reader_bot_owners
  subjects:
    %s: 42
bot_recipients:
  - {id: %s, bot_id: 71, owner_user_id: 99}
  - {id: %s, bot_id: 72, owner_user_id: 99}
`, bot, actor, registeredBotOne, registeredBotTwo)
	if err := yaml.Unmarshal([]byte(source), &access); err != nil {
		t.Fatal(err)
	}
	return &access
}

func TestRegisteredBotsKeepOwnerScopeAndCannotSupplyInputs(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	actor := mm.posts[0].UserID
	w.Access = registeredBotAccess(t, mm.bot, actor)
	audience := []string{mm.bot, actor, registeredBotOne, registeredBotTwo}
	scope, allowed, err := projectAccess(*w.Access, audience, actor)
	if err != nil {
		t.Fatalf("registered receive-only audience rejected: %v", err)
	}
	var readers []int
	if err := json.Unmarshal(scope["reader_user_ids"], &readers); err != nil || !slices.Equal(readers, []int{42, 99}) {
		t.Fatalf("bot owners must participate once in the common data scope: %s, %v", scope["reader_user_ids"], err)
	}
	var owners []struct {
		BotID int `json:"bot_id"`
		Owner int `json:"owner_user_id"`
	}
	if err := json.Unmarshal(scope["reader_bot_owners"], &owners); err != nil || len(owners) != 2 || owners[0].BotID != 71 || owners[1].BotID != 72 || owners[0].Owner != 99 || owners[1].Owner != 99 {
		t.Fatalf("registered bot ownership projection lost: %s, %v", scope["reader_bot_owners"], err)
	}
	if len(allowed) != 2 {
		t.Fatalf("only execution bot and human author may supply inputs: %+v", allowed)
	}
	for _, subject := range allowed {
		if subject.ID == registeredBotOne || subject.ID == registeredBotTwo {
			t.Fatal("receive-only bot acquired input authority")
		}
	}
	for _, bot := range []string{registeredBotOne, registeredBotTwo} {
		if _, _, err := projectAccess(*w.Access, audience, bot); err == nil {
			t.Fatalf("receive-only bot %s became an actor", bot)
		}
	}
}

func TestRegisteredBotsKeepSeparateCorporatePrincipals(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	w.AgentID = "mark"
	w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
	roster := []string{mm.bot, mm.posts[0].UserID, registeredBotOne, registeredBotTwo}
	readers, err := corporateReaders(w, roster)
	if err != nil {
		t.Fatal(err)
	}
	if len(readers) != 3 || readers[1].ID != registeredBotOne || readers[1].BotID != "71" || readers[2].ID != registeredBotTwo || readers[2].BotID != "72" || readers[1].PrincipalID != "99" || readers[2].PrincipalID != "99" {
		t.Fatalf("bots sharing an owner must retain separate confirmed identities: %+v", readers)
	}
}

func TestRegisteredBotRegistryDoesNotRequireAbsentRecipients(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	actor := mm.posts[0].UserID
	w.Access = registeredBotAccess(t, mm.bot, actor)
	for _, roster := range [][]string{{mm.bot, actor}, {mm.bot, actor, registeredBotOne}} {
		scope, _, err := projectAccess(*w.Access, roster, actor)
		if err != nil {
			t.Fatal(err)
		}
		var owners []any
		if len(roster) == 2 {
			if _, exists := scope["reader_bot_owners"]; exists {
				t.Fatal("absent bots entered scope")
			}
		} else if json.Unmarshal(scope["reader_bot_owners"], &owners) != nil || len(owners) != 1 {
			t.Fatal("only current recipients belong to scope")
		}
	}
	w.Access.IdentityProjection.Subjects["nnnnnnnnnnnnnnnnnnnnnnnnnn"] = config.SubjectID(`99`)
	scope, _, err := projectAccess(*w.Access, []string{mm.bot, actor, "nnnnnnnnnnnnnnnnnnnnnnnnnn", registeredBotOne, registeredBotTwo}, actor)
	if err != nil {
		t.Fatal(err)
	}
	var readers []int
	if json.Unmarshal(scope["reader_user_ids"], &readers) != nil || !slices.Equal(readers, []int{42, 99}) {
		t.Fatal("human and bot owner scope duplicated", readers)
	}
}

func TestRegisteredBotCorporateAdmissionAndReconfirmation(t *testing.T) {
	for _, scenario := range []string{"valid", "unknown bot", "inactive bot", "human instead of bot", "owner changed", "registration removed", "bot author", "no mention"} {
		t.Run(scenario, func(t *testing.T) {
			corporateDirectory(t)
			e, a, mm, _, key, w := protectedFixture(t)
			e.SourceID = key.Source
			w.AgentID = "mark"
			w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
			mm.audience = append(mm.audience, registeredBotOne, registeredBotTwo)
			mm.users = map[string]mattermost.User{registeredBotOne: {ID: registeredBotOne, IsBot: true}, registeredBotTwo: {ID: registeredBotTwo, IsBot: true}}
			if scenario == "bot author" {
				mm.posts[0].UserID = registeredBotOne
			}
			if scenario == "no mention" {
				mm.posts[0].Message = "ordinary conversation"
			}
			root := corporateStateBinding(w, key.Channel)
			e.Config.Workflows[0] = root
			var err error
			w, err = root.ForChat(key.Channel)
			if err != nil {
				t.Fatal(err)
			}
			err = e.Thread(t.Context(), w, key, mm.channel, true)
			if scenario == "bot author" || scenario == "no mention" {
				if len(a.attempts) != 0 {
					t.Fatal("receive-only or unmentioned input reached model")
				}
				return
			}
			if err != nil || len(a.attempts) != 1 {
				t.Fatal("registered audience not admitted", err)
			}
			task, err := agentcontract.Decode(*a.attempts[0].Envelope.AccessContext.Assignment)
			if err != nil {
				t.Fatal(err)
			}
			if len(task.Readers) != 3 || task.Readers[1].BotID != "71" || task.Readers[2].BotID != "72" {
				t.Fatal("signed recipients missing", task.Readers)
			}
			if err = e.confirmCorporate(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unknown bot":
				unknown := "zzzzzzzzzzzzzzzzzzzzzzzzzz"
				mm.audience = append(mm.audience, unknown)
				mm.users[unknown] = mattermost.User{ID: unknown, IsBot: true}
			case "inactive bot":
				mm.users[registeredBotOne] = mattermost.User{ID: registeredBotOne, IsBot: true, DeleteAt: 1}
			case "human instead of bot":
				mm.users[registeredBotOne] = mattermost.User{ID: registeredBotOne}
			case "owner changed":
				w.Access.BotRecipients[0].OwnerUserID = config.SubjectID(`100`)
			case "registration removed":
				w.Access.BotRecipients = w.Access.BotRecipients[1:]
			}
			if scenario != "valid" && e.confirmCorporate(t.Context(), task) == nil {
				t.Fatal("changed recipient authority confirmed")
			}
		})
	}
}

func TestRegisteredBotCardCheckConfirmsOnlyHumanSourceAccounts(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	actor := mm.posts[0].UserID
	w.Access = registeredBotAccess(t, mm.bot, actor)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			ID     string `json:"request_id"`
			Method string `json:"method"`
			Params struct {
				Accounts map[string]int `json:"accounts"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "external.confirm_audience" || len(request.Params.Accounts) != 1 || request.Params.Accounts[actor] != 42 {
			t.Error("bot registrations or owners were forged as human source accounts")
			http.Error(rw, "invalid", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"request_id": request.ID, "ok": true, "result": true})
	}))
	defer server.Close()
	original := accountDirectoryHTTP
	accountDirectoryHTTP = server.Client()
	t.Cleanup(func() { accountDirectoryHTTP = original })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", server.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
	roster := []string{mm.bot, actor, registeredBotOne, registeredBotTwo}
	if err := confirmAccountCards(t.Context(), w.Access, roster); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("human cards not confirmed")
	}
	if confirmAccountCards(t.Context(), w.Access, append(roster, strings.Repeat("z", 26))) == nil || calls.Load() != 1 {
		t.Fatal("unknown recipient reached trusted directory")
	}
}

func TestReceiveOnlyBotsDoNotQueueMentionedRequests(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
	post := mm.posts[0]
	for _, id := range []string{registeredBotOne, registeredBotTwo} {
		post.UserID = id
		if candidate(post, mm.channel, w, mattermost.User{ID: mm.bot, Username: "orpheus"}) {
			t.Fatal("registered receive-only bot queued a request")
		}
	}
	post.UserID = mm.posts[0].UserID
	if !candidate(post, mm.channel, w, mattermost.User{ID: mm.bot, Username: "orpheus"}) {
		t.Fatal("human mention no longer starts work")
	}
	post.Message = "ordinary conversation"
	if candidate(post, mm.channel, w, mattermost.User{ID: mm.bot, Username: "orpheus"}) {
		t.Fatal("unmentioned public/private request queued")
	}
	w.Access.Peer = &config.PeerRecipient{ID: registeredBotOne, Field: "peer"}
	w.Access.BotRecipients = nil
	post.UserID, post.Message = registeredBotOne, "@orpheus fixed peer"
	if candidate(post, mm.channel, w, mattermost.User{ID: mm.bot, Username: "orpheus"}) {
		t.Fatal("legacy peer queued a request")
	}
}
