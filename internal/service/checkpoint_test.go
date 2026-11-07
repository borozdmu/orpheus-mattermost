package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestCheckpointConsumesOldIDAndStartsCleanAfterLostAcknowledgement(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	oldSession := a.snapshot.Sessions[0].ID
	oldPost := mm.posts[0]
	mm.posts[0].Message = "@orpheus EDITED_OLD_SECRET"
	mm.posts[0].EditAt = 2000
	mm.posts[0].FileIDs = []string{strings.Repeat("f", 26)}
	// New ID sorts before the old one, at exactly the same millisecond.
	nextID := strings.Repeat("b", 26)
	appendAccessPost(mm, key, nextID, oldPost.UserID, "@orpheus NEW_PERIOD", oldPost.CreateAt)
	w.AdmissionCheckpoint = &config.AdmissionCheckpoint{Schema: "mattermost-admission-checkpoint.v1", ChannelID: key.Channel, BotID: e.Bot.ID, Epoch: "11111111-1111-4111-8111-111111111111", FloorMS: 900, ReplayFromMS: 1000, ConsumedPostIDs: []string{key.Root}}
	w.EffectiveRevision = "new-checkpoint-revision"
	a.lost = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("lost acknowledgement was hidden")
	}
	if len(a.snapshot.Sessions) != 2 || a.snapshot.Sessions[1].ID == oldSession {
		t.Fatal("old sandbox reused")
	}
	attempt := a.attempts[len(a.attempts)-1]
	if len(attempt.Envelope.TriggerIDs) != 1 || attempt.Envelope.TriggerIDs[0] != nextID || attempt.Envelope.Request.PreviousIndex != "" || len(attempt.Envelope.Request.Files) != 0 {
		t.Fatalf("old trigger or file carried: %+v", attempt.Envelope)
	}
	for _, message := range attempt.Messages {
		if strings.Contains(message.Text, "OLD_SECRET") || strings.Contains(message.Text, "ORIGINAL_REQUEST_BODY") {
			t.Fatal("old context carried")
		}
	}
	restarted := &Engine{Config: e.Config, MM: mm, API: a, Sandbox: e.Sandbox, Bot: e.Bot}
	if err := restarted.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 2 {
		t.Fatal("accepted input replayed after restart")
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, strings.Repeat("d", 26), oldPost.UserID, "@orpheus CONTINUE_NEW", 1100)
	if err := restarted.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.snapshot.Sessions) != 2 || a.attempts[2].Session != a.snapshot.Sessions[1].ID {
		t.Fatal("new ownership session did not continue")
	}
}

func TestCheckpointWrongBotFailsBeforePublicationOrAdmission(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	w.AdmissionCheckpoint = &config.AdmissionCheckpoint{ChannelID: key.Channel, BotID: strings.Repeat("z", 26)}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("wrong bot accepted")
	}
	if mm.calls != 0 || len(a.attempts) != 0 {
		t.Fatal("side effect before binding validation")
	}
}

func TestCheckpointDoesNotTransferRejectedInputFromPreviousPeriod(t *testing.T) {
	e, a, _, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.Links.Enabled = new(false)
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	session := &a.snapshot.Sessions[0]
	session.Runs[0].Status = "completed"
	env := a.submitted[0].Envelope
	env.Kind = "clarification"
	session.Messages = append(session.Messages, conversation.Message{ID: "old-input", RunID: session.Runs[0].ID, Role: "user", Text: "OLD_REJECTED_INPUT", Delivery: "rejected", Error: "run_finished_before_delivery", Metadata: testMetadata(env)})
	w.AdmissionCheckpoint = &config.AdmissionCheckpoint{ChannelID: key.Channel, BotID: e.Bot.ID, FloorMS: 900, ReplayFromMS: 1000, ConsumedPostIDs: []string{key.Root}}
	w.EffectiveRevision = "new-period"
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.submitted) != 1 {
		t.Fatal("previous period input replayed")
	}
}

func TestCheckpointBlocksUncertainAdmissionFromPreviousPeriod(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.failure = errors.New("unknown outcome")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("unknown outcome hidden")
	}
	w.AdmissionCheckpoint = &config.AdmissionCheckpoint{ChannelID: key.Channel, BotID: e.Bot.ID}
	w.EffectiveRevision = "new-period"
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("old pending input reused")
	}
	if len(a.attempts) != 1 {
		t.Fatal("admission was retried under new owner")
	}
}
