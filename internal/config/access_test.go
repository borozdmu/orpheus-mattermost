package config

import "testing"

func TestProtectedWorkflow(t *testing.T) {
	base, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	configure := func() Config {
		c := base
		c.Workflows = []Workflow{base.Workflows[0]}
		w := &c.Workflows[0]
		w.Access = &Access{Namespace: "mattermost:test", AgentID: "assistant", PolicySpaceID: "test", BotID: "bbbbbbbbbbbbbbbbbbbbbbbbbb"}
		w.Services = []string{"protected"}
		w.IncludeIDs = []string{"cccccccccccccccccccccccccc"}
		w.PrivateChannels, w.DirectMessages, w.GroupMessages = true, false, false
		w.StartOnMention, w.Links.Enabled = new(true), new(false)
		return c
	}
	c := configure()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Workflows[0].EffectiveRevision == base.Workflows[0].EffectiveRevision {
		t.Fatal("binding did not rotate revision")
	}
	for name, change := range map[string]func(*Workflow){
		"namespace": func(w *Workflow) { w.Access.Namespace = "" },
		"bot":       func(w *Workflow) { w.Access.BotID = "bad" },
		"agent":     func(w *Workflow) { w.Access.AgentID = "" },
		"policy":    func(w *Workflow) { w.Access.PolicySpaceID = "" },
		"links":     func(w *Workflow) { w.Links.Enabled = new(true) },
		"dm":        func(w *Workflow) { w.DirectMessages = true },
		"group":     func(w *Workflow) { w.GroupMessages = true },
		"channels":  func(w *Workflow) { w.IncludeIDs = nil },
		"private":   func(w *Workflow) { w.PrivateChannels = false },
		"services":  func(w *Workflow) { w.Services = nil },
		"mention":   func(w *Workflow) { w.StartOnMention = new(false) },
	} {
		t.Run(name, func(t *testing.T) {
			c := configure()
			change(&c.Workflows[0])
			if c.validate() == nil {
				t.Fatal("unsafe binding accepted")
			}
		})
	}
}
