package config

import (
	"maps"
	"testing"
)

func TestStateRefsRejectAmbiguousOrPartialOwnership(t *testing.T) {
	base := Workflow{ID: "mark-telegram", AgentID: "mark", ChatAccess: map[string]ChatAccess{"first": {StateRef: "old-private"}, "second": {StateRef: "mark-telegram", Deny: true}}}
	if err := ValidateStateRefs([]Workflow{base}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Workflow){
		"duplicate including denied": func(w *Workflow) { v := w.ChatAccess["second"]; v.StateRef = "old-private"; w.ChatAccess["second"] = v },
		"partial migration":          func(w *Workflow) { v := w.ChatAccess["second"]; v.StateRef = ""; w.ChatAccess["second"] = v },
		"path":                       func(w *Workflow) { v := w.ChatAccess["first"]; v.StateRef = "../other"; w.ChatAccess["first"] = v },
		"foreign root":               func(w *Workflow) { v := w.ChatAccess["first"]; v.StateRef = "other-root"; w.ChatAccess["first"] = v },
	} {
		t.Run(name, func(t *testing.T) {
			w := base
			w.ChatAccess = maps.Clone(base.ChatAccess)
			change(&w)
			if ValidateStateRefs([]Workflow{w, {ID: "other-root"}}) == nil {
				t.Fatal("unsafe state ownership accepted")
			}
		})
	}
	other := Workflow{ID: "other-root", ChatAccess: map[string]ChatAccess{"third": {StateRef: "old-private", Deny: true}}}
	if ValidateStateRefs([]Workflow{base, other}) == nil {
		t.Fatal("state partition claimed by another binding")
	}
	changed := base
	changed.ChatAccess = maps.Clone(base.ChatAccess)
	changed.ChatAccess["first"], changed.ChatAccess["second"] = changed.ChatAccess["second"], changed.ChatAccess["first"]
	if SameStateRefs([]Workflow{base}, []Workflow{changed}) {
		t.Fatal("reload moved state between chats")
	}
	if !SameStateRefs([]Workflow{base}, []Workflow{base}) {
		t.Fatal("unchanged state aliases rejected")
	}
	if base.CapacityKey() != (Workflow{ID: "old-private", AgentID: "mark"}).CapacityKey() {
		t.Fatal("state_ref split the shared agent run limit")
	}
}

func TestStateRefResolvesCurrentChannelModeAndKeepsDeny(t *testing.T) {
	c, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := c.Workflows[0]
	w.ID = "mark-mattermost"
	w.AgentID = "mark"
	w.Access = nil
	const chat = "cccccccccccccccccccccccccc"
	const denied = "dddddddddddddddddddddddddd"
	w.IncludeIDs = []string{chat, denied}
	w.ExcludeIDs = nil
	w.PrivateChannels = true
	w.DirectMessages = false
	w.GroupMessages = false
	w.StartOnMention = new(true)
	w.Links.Enabled = new(false)
	w.Services = []string{"protected"}
	w.Memory = true
	w.MemorySharedServices = []string{"protected"}
	w.MemorySharedRevision = "root"
	w.Modes = map[string]ChatMode{"narrow": {Services: []string{"protected"}, Memory: new(false), MemorySharedServices: []string{}, MemorySharedRevision: new("")}}
	w.ChatAccess = map[string]ChatAccess{chat: {StateRef: "mark-heads", Access: accessFixture(), Mode: "narrow"}, denied: {StateRef: "marketing-native", Access: accessFixture(), Deny: true}}
	c.Workflows = []Workflow{w}
	if err = c.validate(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Route(chat, "P")
	if err != nil || got == nil || got.ID != "mark-heads" || got.AgentID != "mark" || got.Memory || len(got.MemorySharedServices) != 0 || got.MemorySharedRevision != "" {
		t.Fatal("old state or current mode lost", err)
	}
	if c.StateWorkflow("mark-heads", denied) != nil || c.StateWorkflow("marketing-native", denied) != nil {
		t.Fatal("cross-channel or denied legacy lookup admitted")
	}
	if got, err = c.Route(denied, "P"); err != nil || got != nil {
		t.Fatal("state_ref opened denied route")
	}
	if len(c.StateWorkflows()) != 1 {
		t.Fatal("denied alias entered recovery")
	}
}
