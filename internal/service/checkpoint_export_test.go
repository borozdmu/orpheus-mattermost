package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

const returnEpoch = "22222222-2222-4222-8222-222222222222"

type exportAPI struct {
	sessions []conversation.Session
	snapshot conversation.Snapshot
	err      error
}

func (a *exportAPI) Sessions(context.Context, string, string) ([]conversation.Session, error) {
	return a.sessions, a.err
}
func (a *exportAPI) Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error) {
	return a.snapshot, a.err
}

type exportSource struct {
	bot   string
	posts []mattermost.Post
	err   error
}

func (m *exportSource) Me(context.Context) (mattermost.User, error) {
	return mattermost.User{ID: m.bot}, m.err
}
func (m *exportSource) Posts(context.Context, string, time.Time) ([]mattermost.Post, error) {
	return m.posts, m.err
}
func (m *exportSource) Thread(context.Context, string) ([]mattermost.Post, error) {
	return m.posts, m.err
}
func (m *exportSource) File(context.Context, string) (mattermost.FileInfo, error) {
	return mattermost.FileInfo{}, errors.New("unexpected file read")
}

func exportFixture(t *testing.T) (config.Workflow, *exportAPI, *exportSource, *Engine, conversation.Key) {
	t.Helper()
	e, api, mm, _, key := fixture(t)
	mm.bot = "1spguz8g9pyhmdrjz7mc9rspbo"
	e.Bot.ID = mm.bot
	w := e.Config.Workflows[0]
	key.Source = w.Mattermost.Source(e.Bot.ID)
	if err := e.Thread(t.Context(), w, key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	s := &api.snapshot.Sessions[0]
	s.ExternalKey = key.External()
	r := &s.Runs[0]
	r.Status = "completed"
	r.FinalMessageID = "final"
	s.Messages = append(s.Messages, conversation.Message{ID: "final", RunID: r.ID, Role: "assistant", Kind: "answer", Text: strings.Repeat("answer ", 20), Position: 2})
	w.Draining = true
	if err := e.Thread(t.Context(), w, key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	w.IncludeIDs = []string{key.Channel}
	w.ExcludeIDs = nil
	w.DirectMessages = false
	w.GroupMessages = false
	w.Links.Enabled = new(false)
	w.Since = time.UnixMilli(200)
	w.AdmissionCheckpoint = &config.AdmissionCheckpoint{Schema: "mattermost-admission-checkpoint.v1", ChannelID: key.Channel, BotID: mm.bot, Epoch: "11111111-1111-4111-8111-111111111111", FloorMS: 100, ReplayFromMS: 200, ConsumedPostIDs: []string{strings.Repeat("p", 26)}}
	return w, &exportAPI{sessions: api.snapshot.Sessions, snapshot: api.snapshot}, &exportSource{bot: mm.bot, posts: mm.posts}, e, key
}

func TestExportPreservesUnreadPostsAndAllConsumedIDs(t *testing.T) {
	w, api, mm, _, key := exportFixture(t)
	// A newer unaccepted mention must remain eligible after return.
	mm.posts = append(mm.posts, mattermost.Post{ID: strings.Repeat("b", 26), ChannelID: key.Channel, UserID: strings.Repeat("h", 26), CreateAt: 9999, Message: "@orpheus unread"})
	first, err := ExportCheckpoint(t.Context(), w, api, mm, returnEpoch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExportCheckpoint(t.Context(), w, api, mm, returnEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.ReplayFromMS != 200 || !reflect.DeepEqual(first.ConsumedPostIDs, []string{strings.Repeat("p", 26), key.Root}) {
		t.Fatalf("bad checkpoint: %+v", first)
	}
	raw, readErr := os.ReadFile("../config/testdata/channel-return-checkpoint.json")
	if readErr != nil {
		t.Fatal(readErr)
	}
	var fixture config.AdmissionCheckpoint
	if err := json.Unmarshal(raw, &fixture); err != nil || !reflect.DeepEqual(fixture, first) {
		t.Fatal("shared return fixture mismatch", err, first)
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), "answer") || strings.Contains(string(encoded), api.sessions[0].ID) {
		t.Fatal("private content exported")
	}
}

func TestExportFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Workflow, *exportAPI, *exportSource)
	}{
		{"not drained", func(w *config.Workflow, _ *exportAPI, _ *exportSource) { w.Draining = false }},
		{"wrong bot", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.bot = strings.Repeat("z", 26) }},
		{"missing final", func(_ *config.Workflow, a *exportAPI, _ *exportSource) {
			a.snapshot.Sessions[0].Runs[0].FinalMessageID = "missing"
		}},
		{"active", func(_ *config.Workflow, a *exportAPI, _ *exportSource) {
			a.snapshot.Sessions[0].Runs[0].Status = "running"
		}},
		{"pending input", func(_ *config.Workflow, a *exportAPI, _ *exportSource) {
			a.snapshot.Sessions[0].Messages[0].Delivery = "pending"
		}},
		{"unknown metadata", func(_ *config.Workflow, a *exportAPI, _ *exportSource) {
			for i := range a.snapshot.Sessions[0].Messages {
				m := &a.snapshot.Sessions[0].Messages[i]
				if conversation.HasMetadata(m.Metadata) {
					m.Metadata = append([]byte(`{"future":true,`), m.Metadata[1:]...)
				}
			}
		}},
		{"missing receipt", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.posts = m.posts[:1] }},
		{"edited receipt", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.posts[1].EditAt = 300 }},
		{"deleted receipt", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.posts[1].DeleteAt = 300 }},
		{"duplicate receipt", func(_ *config.Workflow, _ *exportAPI, m *exportSource) {
			p := m.posts[1]
			p.ID = strings.Repeat("z", 26)
			m.posts = append(m.posts, p)
		}},
		{"forged receipt", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.posts[1].UserID = strings.Repeat("z", 26) }},
		{"changed text", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.posts[1].Message = "edited" }},
		{"missing session", func(_ *config.Workflow, a *exportAPI, _ *exportSource) { a.snapshot.Sessions = nil }},
		{"history unavailable", func(_ *config.Workflow, a *exportAPI, _ *exportSource) { a.err = errors.New("incomplete history") }},
		{"thread unavailable", func(_ *config.Workflow, _ *exportAPI, m *exportSource) { m.err = errors.New("incomplete thread") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, a, m, _, _ := exportFixture(t)
			tc.change(&w, a, m)
			cp, err := ExportCheckpoint(t.Context(), w, a, m, returnEpoch)
			if err == nil || cp.Schema != "" {
				t.Fatal("uncertainty exported", cp, err)
			}
		})
	}
}

func TestExportIncludesPreAdmissionRejectionAndRequiresEveryPart(t *testing.T) {
	w, a, m, e, key := exportFixture(t)
	source := &fakeSource{bot: m.bot, posts: m.posts, calls: len(m.posts)}
	p := &delivery.Publisher{MM: source, Key: key, BotID: m.bot}
	id := strings.Repeat("n", 26)
	env := conversation.Envelope{Anchor: id, TriggerIDs: []string{id}, Revision: "revision", Render: conversation.Render{Version: 4, MaxChars: 64}, Versions: []conversation.Version{{ID: id, Hash: "hash"}}}
	if err := e.reject(t.Context(), p, env, []conversation.InputMessage{{Text: "private request"}}, strings.Repeat("rejected ", 30)); err != nil {
		t.Fatal(err)
	}
	m.posts = source.posts
	cp, err := ExportCheckpoint(t.Context(), w, a, m, returnEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cp.ConsumedPostIDs, []string{id, strings.Repeat("p", 26), key.Root}) {
		t.Fatal(cp)
	}
	// A rejection-only thread has no Orpheus session at all.
	a.sessions = nil
	a.snapshot.Sessions = nil
	m.posts = append(m.posts[:1:1], m.posts[2:]...)
	cp, err = ExportCheckpoint(t.Context(), w, a, m, returnEpoch)
	if err != nil || !reflect.DeepEqual(cp.ConsumedPostIDs, []string{id, strings.Repeat("p", 26)}) {
		t.Fatal("rejection without session lost", cp, err)
	}
	m.posts = m.posts[:len(m.posts)-1]
	if _, err = ExportCheckpoint(t.Context(), w, a, m, returnEpoch); err == nil {
		t.Fatal("partial rejection exported")
	}
}
