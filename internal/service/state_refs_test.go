package service

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func corporateStateBinding(w config.Workflow, chat string) config.Workflow {
	old := w.ID
	w.ID = "mark-mattermost"
	w.PrivateChannels = true
	w.AgentID = "mark"
	w.ChatAccess = map[string]config.ChatAccess{chat: {StateRef: old, Access: w.Access}}
	w.IncludeIDs = []string{chat}
	w.Access = nil
	return w
}

func TestCorporateStateRefRecoversOldSessionAndOperation(t *testing.T) {
	corporateDirectory(t)
	e, api, mm, _, key, w := protectedFixture(t)
	e.SourceID = key.Source
	w.Access.VerifyAccountCards = true
	w.Access.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	w.Access.IdentityProjection.ActorField = "actor_user_id"
	w.Access.IdentityProjection.AudienceField = "reader_user_ids"
	api.lost = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("expected lost response")
	}
	if len(api.attempts) != 1 {
		t.Fatal("old admission missing")
	}
	original := api.attempts[0]
	root := corporateStateBinding(w, key.Channel)
	e.Config.Workflows = []config.Workflow{root}
	current, err := e.Config.Route(key.Channel, mm.channel.Type)
	if err != nil || current == nil || current.ID != key.Workflow || current.AgentID != "mark" {
		t.Fatal("old partition was not resolved", err)
	}
	if current.ID == root.ID || original.Envelope.Key().Namespace() != key.Namespace() || original.Envelope.Key().Operation(original.Envelope.Anchor, "initial", "", "") != key.Operation(original.Envelope.Anchor, "initial", "", "") {
		t.Fatal("namespace or operation partition changed")
	}
	// A new Engine has no pending in-memory submission; accepted native history
	// is the only authority for recovering the lost response.
	restarted := &Engine{Config: e.Config, MM: e.MM, API: api, Sandbox: e.Sandbox, Bot: e.Bot, SourceID: e.SourceID}
	if err = restarted.Thread(t.Context(), *current, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.attempts) != 1 {
		t.Fatal("migration duplicated accepted input")
	}
	if len(api.snapshot.Sessions) != 1 {
		t.Fatal("old session disappeared")
	}
}

type stateDiscoveryAPI struct {
	API
	namespaces []string
	sessions   []conversation.Session
}

func (a *stateDiscoveryAPI) Sessions(_ context.Context, namespace, _ string) ([]conversation.Session, error) {
	a.namespaces = append(a.namespaces, namespace)
	return a.sessions, nil
}

func TestCorporateStateRefDiscoveryAndHintsUseExactChannel(t *testing.T) {
	e, _, _, _, key, w := protectedFixture(t)
	root := corporateStateBinding(w, key.Channel)
	root.Draining = true
	e.Config.Workflows = []config.Workflow{root}
	e.SourceID = key.Source
	api := &stateDiscoveryAPI{sessions: []conversation.Session{{ID: "old-session", ExternalKey: key.External()}, {ID: "foreign-session", ExternalKey: conversation.Key{Source: key.Source, Workflow: key.Workflow, Channel: strings.Repeat("z", 26), Root: key.Root}.External()}}}
	source := &membershipSource{channels: []mattermost.Channel{{ID: key.Channel, Type: "P"}}}
	e.MM = source
	e.API = api
	r := &Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler), cfg: e.Config, jobs: map[conversation.Key]*scheduled{}, channels: map[string]mattermost.Channel{}, active: map[conversation.Key]int{}, unknown: map[conversation.Key]bool{}, scanned: map[string]time.Time{}, discovered: map[string]time.Time{}, digests: map[conversation.Key]string{}, wake: make(chan struct{}, 1), scanning: true, discovering: true}
	r.scan(t.Context(), e.Config, nil, 0)
	if !r.ready.Load() || len(api.namespaces) != 1 || api.namespaces[0] != key.Namespace() || len(r.jobs) != 1 || r.jobs[key] == nil {
		t.Fatal("old namespace discovery missing or crossed channel", api.namespaces)
	}
	delete(r.jobs, key)
	r.hint(key)
	if r.jobs[key] == nil || r.jobs[key].Workflow.AgentID != "mark" {
		t.Fatal("legacy SSE hint lost current role")
	}
	root.ChatAccess = maps.Clone(root.ChatAccess)
	entry := root.ChatAccess[key.Channel]
	entry.Deny = true
	root.ChatAccess[key.Channel] = entry
	r.cfg.Workflows = []config.Workflow{root}
	delete(r.jobs, key)
	r.hint(key)
	if len(r.jobs) != 0 {
		t.Fatal("denied alias admitted by hint")
	}
	changed := e.Config
	changed.Workflows = []config.Workflow{root}
	entry.StateRef = "other-state"
	root.ChatAccess[key.Channel] = entry
	changed.Workflows = []config.Workflow{root}
	if reloadCompatible(e.Config, changed) {
		t.Fatal("reload moved state partition")
	}
}

func TestCorporateStateRefSharesAgentCapacity(t *testing.T) {
	_, _, _, _, key, w := protectedFixture(t)
	w.MaxConcurrentRuns = 1
	root := corporateStateBinding(w, key.Channel)
	second := key
	second.Channel = strings.Repeat("q", 26)
	second.Workflow = "second-old-state"
	root.IncludeIDs = append(root.IncludeIDs, second.Channel)
	root.ChatAccess[second.Channel] = config.ChatAccess{StateRef: second.Workflow, Access: w.Access}
	cfg := config.Config{Workflows: []config.Workflow{root}}
	r := &Runtime{cfg: cfg, initialized: true, active: map[conversation.Key]int{key: 1}, unknown: map[conversation.Key]bool{}, jobs: map[conversation.Key]*scheduled{second: {}}}
	child := cfg.StateWorkflow(second.Workflow, second.Channel)
	if child == nil || r.reserve(second, *child) {
		t.Fatal("state aliases split common agent capacity")
	}
	delete(r.active, key)
	r.unknown[key] = true
	if r.reserve(second, *child) {
		t.Fatal("unknown old partition did not reserve agent capacity")
	}
}

func TestCorporateDiscoveryWithoutAliasesKeepsAllChannels(t *testing.T) {
	e, _, _, _, key, w := protectedFixture(t)
	root := corporateStateBinding(w, key.Channel)
	root.Draining = true
	other := key
	other.Channel = strings.Repeat("q", 26)
	first := root.ChatAccess[key.Channel]
	first.StateRef = ""
	root.ChatAccess = map[string]config.ChatAccess{key.Channel: first, other.Channel: first}
	root.IncludeIDs = append(root.IncludeIDs, other.Channel)
	key.Workflow, other.Workflow = root.ID, root.ID
	e.Config.Workflows = []config.Workflow{root}
	e.SourceID = key.Source
	api := &stateDiscoveryAPI{sessions: []conversation.Session{{ID: "first", ExternalKey: key.External()}, {ID: "second", ExternalKey: other.External()}}}
	e.API = api
	e.MM = &membershipSource{channels: []mattermost.Channel{{ID: key.Channel, Type: "P"}, {ID: other.Channel, Type: "P"}}}
	r := &Runtime{Engine: e, Logger: slog.New(slog.DiscardHandler), cfg: e.Config, jobs: map[conversation.Key]*scheduled{}, channels: map[string]mattermost.Channel{}, active: map[conversation.Key]int{}, unknown: map[conversation.Key]bool{}, scanned: map[string]time.Time{}, discovered: map[string]time.Time{}, digests: map[conversation.Key]string{}, wake: make(chan struct{}, 1), scanning: true, discovering: true}
	r.scan(t.Context(), e.Config, nil, 0)
	if !r.ready.Load() || len(r.jobs) != 2 || r.jobs[key] == nil || r.jobs[other] == nil {
		t.Fatal("shared new namespace lost another channel during recovery")
	}
}

func TestDeniedLegacyPartitionStillHoldsAgentCapacity(t *testing.T) {
	_, _, _, _, first, w := protectedFixture(t)
	w.MaxConcurrentRuns = 1
	root := corporateStateBinding(w, first.Channel)
	second := first
	second.Channel = strings.Repeat("q", 26)
	second.Workflow = "second-old-state"
	root.IncludeIDs = append(root.IncludeIDs, second.Channel)
	root.ChatAccess[second.Channel] = config.ChatAccess{StateRef: second.Workflow, Access: w.Access}
	entry := root.ChatAccess[first.Channel]
	entry.Deny = true
	root.ChatAccess[first.Channel] = entry
	cfg := config.Config{Workflows: []config.Workflow{root}}
	current := cfg.StateWorkflow(second.Workflow, second.Channel)
	if current == nil || cfg.StateWorkflow(first.Workflow, first.Channel) != nil {
		t.Fatal("current route or denied state boundary invalid")
	}
	for _, unknown := range []bool{false, true} {
		r := &Runtime{cfg: cfg, initialized: true, active: map[conversation.Key]int{}, unknown: map[conversation.Key]bool{}, jobs: map[conversation.Key]*scheduled{second: {}}}
		if unknown {
			r.unknown[first] = true
		} else {
			r.active[first] = 1
		}
		if r.reserve(second, *current) {
			t.Errorf("new run admitted before denied old partition settled (unknown=%v)", unknown)
		}
		delete(r.active, first)
		delete(r.unknown, first)
		if !r.reserve(second, *current) {
			t.Fatal("settled old partition retained capacity")
		}
	}
}
