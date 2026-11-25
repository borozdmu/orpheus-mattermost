package config

import (
	"strings"
	"testing"
)

func botRecipientsFixture() *Access {
	a := accessFixture()
	a.VerifyAccountCards = true
	a.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	a.IdentityProjection.BotOwnersField = "reader_bot_owners"
	a.BotRecipients = []BotRecipient{
		{ID: strings.Repeat("p", 26), BotID: SubjectID(`71`), OwnerUserID: SubjectID(`77`)},
		{ID: strings.Repeat("q", 26), BotID: SubjectID(`72`), OwnerUserID: SubjectID(`77`)},
	}
	return a
}

func TestRegisteredBotRecipientValidation(t *testing.T) {
	if err := botRecipientsFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Access){
		"unconfirmed cards":            func(a *Access) { a.VerifyAccountCards = false },
		"no agent":                     func(a *Access) { delete(a.ScopeContext, "agent_id") },
		"blank agent":                  func(a *Access) { a.ScopeContext["agent_id"] = " " },
		"legacy peer":                  func(a *Access) { a.Peer = &PeerRecipient{ID: strings.Repeat("r", 26), Field: "peer"} },
		"channel-only":                 func(a *Access) { a.ChannelOnly = true },
		"local services":               func(a *Access) { a.LocalServices = true },
		"invalid source":               func(a *Access) { a.BotRecipients[0].ID = "bad" },
		"execution bot":                func(a *Access) { a.BotRecipients[0].ID = a.BotID },
		"human source":                 func(a *Access) { a.BotRecipients[0].ID = strings.Repeat("h", 26) },
		"duplicate source":             func(a *Access) { a.BotRecipients[1].ID = a.BotRecipients[0].ID },
		"duplicate registration":       func(a *Access) { a.BotRecipients[1].BotID = a.BotRecipients[0].BotID },
		"duplicate typed registration": func(a *Access) { a.BotRecipients[1].BotID = SubjectID(`"71"`) },
		"missing bot registration":     func(a *Access) { a.BotRecipients[0].BotID = nil },
		"missing owner":                func(a *Access) { a.BotRecipients[0].OwnerUserID = nil },
		"invalid owner":                func(a *Access) { a.BotRecipients[0].OwnerUserID = SubjectID(`true`) },
		"no ownership field":           func(a *Access) { a.IdentityProjection.BotOwnersField = "" },
		"invalid ownership field":      func(a *Access) { a.IdentityProjection.BotOwnersField = "nested.bots" },
		"actor field collision":        func(a *Access) { a.IdentityProjection.BotOwnersField = a.IdentityProjection.ActorField },
		"audience field collision":     func(a *Access) { a.IdentityProjection.BotOwnersField = a.IdentityProjection.AudienceField },
		"static field collision":       func(a *Access) { a.ScopeContext[a.IdentityProjection.BotOwnersField] = []int{71} },
		"field without registry":       func(a *Access) { a.BotRecipients = nil },
		"oversized registry":           func(a *Access) { a.BotRecipients = make([]BotRecipient, 1001) },
	} {
		t.Run(name, func(t *testing.T) {
			a := botRecipientsFixture()
			change(a)
			if err := a.Validate(); err == nil {
				t.Fatal("unsafe recipient registry accepted")
			}
		})
	}
}

func TestRegisteredBotRecipientsRequireCorporateWorkflowAndNoTriggers(t *testing.T) {
	for _, scenario := range []string{"valid", "no corporate agent", "different agent", "bot trigger"} {
		t.Run(scenario, func(t *testing.T) {
			c, err := example(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.Workflows = c.Workflows[:1]
			w := &c.Workflows[0]
			w.Access = botRecipientsFixture()
			w.AgentID = "mark"
			w.Services = []string{"protected"}
			w.IncludeIDs = []string{strings.Repeat("c", 26)}
			w.PrivateChannels, w.DirectMessages, w.GroupMessages = true, false, false
			w.StartOnMention, w.Links.Enabled = new(true), new(false)
			switch scenario {
			case "no corporate agent":
				w.AgentID = ""
			case "different agent":
				w.AgentID = "another"
			case "bot trigger":
				w.TriggerBotIDs = []string{w.Access.BotRecipients[0].ID}
			}
			if err := c.validate(); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestRegisteredBotOwnershipChangesRotateAuthority(t *testing.T) {
	a := botRecipientsFixture()
	original, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	a.BotRecipients[0].OwnerUserID = SubjectID(`88`)
	changed, err := a.Digest()
	if err != nil || changed == original {
		t.Fatal("owner change retained authority", err)
	}
	if a.RegisteredBot(strings.Repeat("p", 26)) == nil || !a.ReceiveOnly(strings.Repeat("q", 26)) || a.ReceiveOnly(strings.Repeat("h", 26)) {
		t.Fatal("receive-only lookup lost source identity")
	}
	var absent *Access
	if absent.RegisteredBot("unknown") != nil || absent.ReceiveOnly("unknown") {
		t.Fatal("nil binding has recipients")
	}
}
