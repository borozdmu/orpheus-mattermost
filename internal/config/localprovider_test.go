package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func localProviderWorkflow(t *testing.T) (Config, *Workflow) {
	t.Helper()
	c, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Workflows = c.Workflows[:1]
	w := &c.Workflows[0]
	w.Access = accessFixture()
	w.Access.LocalServices, w.Access.LocalProviderContext, w.Access.LocalFiles = true, true, true
	w.Access.Peer = &PeerRecipient{ID: strings.Repeat("p", 26), Field: "peer"}
	w.Access.ScopeContext["policy_space_id"] = "space." + w.Access.Peer.ID
	w.IncludeIDs = []string{strings.Repeat("c", 26)}
	w.PrivateChannels, w.GroupMessages, w.DirectMessages = true, false, false
	w.Services, w.EnvFrom, w.TriggerBotIDs = []string{"local-memory", "kord"}, nil, nil
	w.StartOnMention, w.Links.Enabled = new(true), new(false)
	return c, w
}

func TestLocalProviderScopeValidation(t *testing.T) {
	for _, scenario := range []string{"valid", "no peer", "wrong peer", "missing space", "non-string space", "no local services", "channel only"} {
		t.Run(scenario, func(t *testing.T) {
			c, w := localProviderWorkflow(t)
			switch scenario {
			case "no peer":
				w.Access.Peer = nil
			case "wrong peer":
				w.Access.ScopeContext["policy_space_id"] = "space.other"
			case "missing space":
				delete(w.Access.ScopeContext, "policy_space_id")
			case "non-string space":
				w.Access.ScopeContext["policy_space_id"] = 12
			case "no local services":
				w.Access.LocalServices = false
			case "channel only":
				w.Access.ChannelOnly = true
			}
			if err := c.validate(); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestLocalBeforeRunPythonValidationAndRevision(t *testing.T) {
	for _, scenario := range []string{"valid", "large", "NUL", "no access", "no local services", "encoded hook too large"} {
		t.Run(scenario, func(t *testing.T) {
			c, w := localProviderWorkflow(t)
			w.LocalBeforeRunPython = "print('trusted')"
			switch scenario {
			case "large":
				w.LocalBeforeRunPython = strings.Repeat("#", (64<<10)+1)
			case "NUL":
				w.LocalBeforeRunPython = "a\x00b"
			case "no access":
				w.Access = nil
			case "no local services":
				w.Access.LocalProviderContext = false
				w.Access.LocalFiles = false
				w.Access.LocalServices = false
			case "encoded hook too large":
				w.LocalBeforeRunPython = strings.Repeat("#", 64<<10)
			}
			if err := c.validate(); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
	c, w := localProviderWorkflow(t)
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	old := w.EffectiveRevision
	w.LocalBeforeRunPython = "print('trusted')"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if old == w.EffectiveRevision {
		t.Fatal("bootstrap change retained old session revision")
	}
	raw, err := json.Marshal(Workflow{})
	if err != nil || strings.Contains(string(raw), "local_before_run_python") {
		t.Fatal("default changes serialized workflow", err)
	}
	raw, err = json.Marshal(Access{})
	if err != nil || strings.Contains(string(raw), "local_provider_context") {
		t.Fatal("default changes serialized access", err)
	}
}
