package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

type accessSource struct {
	*fakeSource
	channel          mattermost.Channel
	audience         []string
	reads, fileReads int
	fail             bool
}

func (s *accessSource) Audience(context.Context, string) ([]string, error) {
	s.reads++
	if s.fail {
		return nil, errors.New("audience unavailable")
	}
	return slices.Clone(s.audience), nil
}
func (s *accessSource) Channel(context.Context, string) (mattermost.Channel, error) {
	return s.channel, nil
}
func (s *accessSource) Files(ctx context.Context, id string) ([]mattermost.FileInfo, error) {
	s.fileReads++
	return s.fakeSource.Files(ctx, id)
}

type accessAPI struct {
	*fakeAPI
	failure  error
	attempts []pending
}

func (a *accessAPI) Submit(ctx context.Context, w config.Workflow, env conversation.Envelope, messages []conversation.InputMessage, sid, rid, pred string) (conversation.Accepted, error) {
	a.attempts = append(a.attempts, pending{Workflow: w, Envelope: env, Messages: messages, Session: sid, Run: rid, Predecessor: pred})
	if a.failure != nil {
		err := a.failure
		a.failure = nil
		return conversation.Accepted{}, err
	}
	return a.fakeAPI.Submit(ctx, w, env, messages, sid, rid, pred)
}
func protectedFixture(t *testing.T) (*Engine, *accessAPI, *accessSource, *fakeSandbox, conversation.Key, config.Workflow) {
	t.Helper()
	e, a, mm, box, key := fixture(t)
	w := e.Config.Workflows[0]
	w.Access = &config.Access{Namespace: "mattermost:test", AgentID: "assistant", PolicySpaceID: "test", BotID: e.Bot.ID}
	w.IncludeIDs = []string{key.Channel}
	w.Links.Enabled = new(false)
	source := &accessSource{fakeSource: mm, channel: mattermost.Channel{ID: key.Channel, Type: "P"}, audience: []string{mm.bot, mm.posts[0].UserID}}
	api := &accessAPI{fakeAPI: a}
	e.MM, e.API = source, api
	return e, api, source, box, key, w
}
func completeAccessRun(a *accessAPI) {
	s := &a.snapshot.Sessions[len(a.snapshot.Sessions)-1]
	s.Runs[len(s.Runs)-1].Status = "completed"
}
func appendAccessPost(s *accessSource, key conversation.Key, id, author, text string, at int64, files ...string) {
	s.posts = append(s.posts, mattermost.Post{ID: id, RootID: key.Root, ChannelID: key.Channel, UserID: author, Message: text, CreateAt: at, FileIDs: files})
}
func TestProtectedAdmissionFrozenAndRecovered(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.failure = &orpheus.Error{Status: 429, Code: "capacity_exceeded"}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("capacity error lost")
	}
	first := a.attempts[0]
	mm.audience = append(mm.audience, "nnnnnnnnnnnnnnnnnnnnnnnnnn")
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if mm.reads != 1 || !reflect.DeepEqual(first.Envelope, a.attempts[1].Envelope) {
		t.Fatal("retry refreshed snapshot")
	}
	// Restart after acceptance recovers through authoritative history, even if MM
	// membership is unavailable. It must not issue the same invocation again.
	e.pending = nil
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 2 || mm.reads != 1 {
		t.Fatal("duplicate accepted run")
	}
}
func TestProtectedAdmissionWaitsForSlot(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	e.Reserve = func(conversation.Key, config.Workflow) bool { return false }
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if mm.reads != 0 || len(a.attempts) != 0 {
		t.Fatal("snapshot taken while waiting")
	}
	e.Reserve = nil
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil || len(a.attempts) != 0 {
		t.Fatal("failed audience admitted")
	}
}
func TestActiveAuthorFilteredBeforeFiles(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	outsider := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", outsider, "@orpheus outsider", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || box.prepared != 0 || mm.fileReads != 0 {
		t.Fatal("outsider file crossed active boundary")
	}
	appendAccessPost(mm, key, "tttttttttttttttttttttttttt", mm.audience[1], "@orpheus allowed", 5000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	submitted := a.attempts[len(a.attempts)-1]
	if submitted.Run == "" || len(submitted.Envelope.InputSubjects) != len(submitted.Messages) || strings.Contains(joinedInput(submitted.Messages), "outsider") || mm.reads != 1 {
		t.Fatal("invalid active batch")
	}
	completeAccessRun(a)
	mm.audience = append(mm.audience, outsider)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if a.attempts[len(a.attempts)-1].Envelope.Anchor != "ssssssssssssssssssssssssss" || mm.reads != 2 {
		t.Fatal("excluded trigger did not remain pending")
	}
}
func TestScopeChangedCreatesCleanSessionWithSameInvocation(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "pppppppppppppppppppppppppp", mm.audience[1], "OLD_PRIVATE_CONTEXT", 2000, "ffffffffffffffffffffffffff")
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus current", 4000, "gggggggggggggggggggggggggg")
	a.failure = &orpheus.Error{Status: 409, Code: "service_scope_changed"}
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	before, after := a.attempts[1], a.attempts[2]
	if before.Session == "" || after.Session != "" || !reflect.DeepEqual(before.Envelope.AccessContext, after.Envelope.AccessContext) || mm.reads != 2 {
		t.Fatal("scope reset reissued identity")
	}
	if after.Envelope.Request.PreviousIndex != "" || len(after.Envelope.Request.DeliveredBatches) != 0 || strings.Contains(joinedInput(after.Messages), "OLD_PRIVATE_CONTEXT") || len(after.Envelope.Request.Files) != 1 || after.Envelope.ContextStart.PostID != after.Envelope.Anchor {
		t.Fatal("old context crossed reset", after.Envelope)
	}
	// A subsequent process inherits the new history boundary, even after deletion
	// of the boundary post; only the new session remains reusable.
	completeAccessRun(a)
	e.pending = nil
	mm.posts = slices.DeleteFunc(mm.posts, func(p mattermost.Post) bool { return p.ID == after.Envelope.Anchor })
	appendAccessPost(mm, key, "tttttttttttttttttttttttttt", mm.audience[1], "@orpheus later", 6000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	last := a.attempts[len(a.attempts)-1]
	if strings.Contains(joinedInput(last.Messages), "OLD_PRIVATE_CONTEXT") || last.Envelope.ContextStart.PostID != after.Envelope.Anchor {
		t.Fatal("boundary lost on restart")
	}
}
func TestOnlyScopeConflictResets(t *testing.T) {
	for _, failure := range []*orpheus.Error{{Status: 503, Code: "service_scope_changed"}, {Status: 409, Code: "session_busy"}} {
		t.Run(failure.Code+string(rune(failure.Status)), func(t *testing.T) {
			e, a, mm, _, key, w := protectedFixture(t)
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
				t.Fatal(err)
			}
			completeAccessRun(a)
			appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus next", 4000)
			a.failure = failure
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
				t.Fatal("failure swallowed")
			}
			if len(a.attempts) != 2 || len(a.snapshot.Sessions) != 1 {
				t.Fatal("unexpected new session")
			}
		})
	}
}
func TestLostProtectedAdmissionDoesNotReset(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.lost = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err == nil {
		t.Fatal("lost response not simulated")
	}
	e.pending = nil
	mm.fail = true
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 1 || len(a.snapshot.Sessions) != 1 {
		t.Fatal("duplicate invocation")
	}
}
func TestUnknownProvenanceStartsClean(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	unprotected := w
	unprotected.Access = nil
	if err := e.Thread(t.Context(), unprotected, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus next", 4000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	last := a.attempts[1]
	if last.Session != "" || len(last.Messages) != 1 || last.Envelope.Request.PreviousIndex != "" {
		t.Fatal("unknown memory resumed")
	}
}

func TestAnotherAuthorKeepsCompatibleSession(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	other := "nnnnnnnnnnnnnnnnnnnnnnnnnn"
	mm.audience = append(mm.audience, other)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", other, "@orpheus continue", 4000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.snapshot.Sessions) != 1 || a.attempts[1].Session != a.snapshot.Sessions[0].ID || a.attempts[1].Envelope.AccessContext.Subject.ID != other || a.attempts[1].Envelope.AccessContext.InvocationID == a.attempts[0].Envelope.AccessContext.InvocationID {
		t.Fatal("author change lost compatible session")
	}
}

func TestNoMentionDoesNotStartProtectedRun(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	mm.posts[0].Message = "passive context"
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 0 || mm.reads != 0 {
		t.Fatal("passive post started run")
	}
}

func TestProtectedActiveFileKeepsOriginalSnapshot(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	mm.fail = true
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.audience[1], "@orpheus file", 4000, "ffffffffffffffffffffffffff")
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if box.prepared != 1 || mm.reads != 1 || !reflect.DeepEqual(a.attempts[0].Envelope.AccessContext, a.attempts[1].Envelope.AccessContext) || len(a.attempts[1].Envelope.InputSubjects) != 1 {
		t.Fatal("active input changed its run snapshot")
	}
}
