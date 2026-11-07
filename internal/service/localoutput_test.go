package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type localUploadMM struct {
	outputSource
	uploads int
	channel string
	data    []byte
}

func (m *localUploadMM) Upload(_ context.Context, channel, _ string, reader io.Reader) (mattermost.FileInfo, error) {
	m.uploads++
	m.channel = channel
	data, err := io.ReadAll(reader)
	m.data = data
	m.file = mattermost.FileInfo{ID: strings.Repeat("f", 26)}
	return m.file, err
}

type localRoundTrip func(*http.Request) (*http.Response, error)

func (f localRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLocalOutputPublicationAndRestart(t *testing.T) {
	for _, scenario := range []string{"success", "bad-hash", "foreign-run", "foreign-file-id", "audience-revoked", "missing-hook", "running-hook", "failed-hook", "truncated-hook", "unsealed"} {
		t.Run(scenario, func(t *testing.T) {
			data := []byte("Крокус [EMAIL_ADDRESS_synthetic]")
			file := attachments.Artifact{ArtifactID: strings.Repeat("a", 64), Name: "document-01.text-version.txt", SHA256: attachments.Hash(data), Size: int64(len(data))}
			sid, rid := "session", "run"
			manifest := localManifest{Schema: 1, SessionID: sid, RunID: rid, Stage: "sealed", Files: []attachments.Artifact{file}}
			run := conversation.Run{ID: rid, Status: "completed", Hooks: []conversation.Hook{{Name: "after_run", Status: "completed", Completeness: "complete"}}}
			if scenario == "foreign-run" {
				manifest.RunID = "different"
			}
			if scenario == "foreign-file-id" {
				manifest.Files[0].FileID = "foreign"
			}
			if scenario == "unsealed" {
				manifest.Stage = "staging"
			}
			if scenario == "missing-hook" {
				run.Hooks = nil
			}
			switch scenario {
			case "running-hook":
				run.Hooks[0].Status = "running"
			case "failed-hook":
				run.Hooks[0].Status = "failed"
			case "truncated-hook":
				run.Hooks[0].Completeness = "truncated"
			}
			mm := &localUploadMM{fakeSource: &fakeSource{bot: "bot"}}
			e := &Engine{MM: mm}
			p := &delivery.Publisher{MM: mm, BotID: "bot", Key: conversation.Key{Source: "source", Workflow: "legal", Channel: "channel", Root: "root"}}
			gates := 0
			p.BeforeCreate = func(context.Context) error {
				gates++
				if scenario == "audience-revoked" {
					return errors.New("denied")
				}
				return nil
			}
			relayCalls := 0
			client := &http.Client{Transport: localRoundTrip(func(req *http.Request) (*http.Response, error) {
				relayCalls++
				var b []byte
				switch req.URL.Path {
				case "/manifest":
					b, _ = json.Marshal(manifest)
				case "/read":
					b = data
					if scenario == "bad-hash" {
						b = bytes.Repeat([]byte("x"), len(data))
					}
				default:
					t.Fatal("unexpected relay operation")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
			})}
			env := conversation.Envelope{Channel: "channel", Request: attachments.Request{Limits: config.Files{MaxOutputFiles: 15, MaxOutputBytes: 4 << 20}}}
			session := conversation.Session{ID: sid}
			out, err := e.projectLocalOutput(t.Context(), p, session, run, env, client)
			if scenario != "success" {
				if err == nil || mm.uploads != 0 {
					t.Fatal("unsafe output uploaded", err, mm.uploads)
				}
				if strings.HasSuffix(scenario, "-hook") && relayCalls != 0 {
					t.Fatal("incomplete hook reached the relay", relayCalls)
				}
				return
			}
			if err != nil || len(out.Files) != 1 || !bytes.Equal(mm.data, data) || mm.channel != "channel" || gates != 1 {
				t.Fatal("export failed", out, err, gates)
			}
			parts := delivery.Parts(p.Key, sid, rid, "answer", "Ready", conversation.Render{Version: 1, MaxChars: 100}, out.Files)
			for _, part := range parts {
				if err := p.Publish(t.Context(), part); err != nil {
					t.Fatal(err)
				}
			}
			recovered := &delivery.Publisher{MM: mm, BotID: p.BotID, Key: p.Key, BeforeCreate: p.BeforeCreate}
			if err := recovered.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = e.projectLocalOutput(t.Context(), recovered, session, run, env, client); err != nil {
				t.Fatal(err)
			}
			if mm.uploads != 1 || mm.calls != 1 || gates != 2 {
				t.Fatal("duplicate delivery or missing audience check", mm.uploads, mm.calls, gates)
			}
		})
	}
}

func TestOldLocalRunReplayDoesNotAcquireFileExport(t *testing.T) {
	e, _, mm, _, key := fixture(t)
	env := input("initial")
	env.Source, env.Workflow, env.Channel, env.Root = key.Source, key.Workflow, key.Channel, key.Root
	run := conversation.Run{ID: "run", Status: "completed", AgentStatus: "completed", FinalMessageID: "answer"}
	session := conversation.Session{ID: "session", Messages: []conversation.Message{
		{RunID: run.ID, Role: "user", Metadata: testMetadata(env)},
		{ID: "answer", RunID: run.ID, Role: "assistant", Kind: "answer", Text: "Already delivered", Position: 1},
	}}
	p := &delivery.Publisher{MM: mm, BotID: mm.bot, Key: key}
	for _, part := range delivery.Parts(key, session.ID, run.ID, "answer", "Already delivered", env.Render, nil) {
		if err := p.Publish(t.Context(), part); err != nil {
			t.Fatal(err)
		}
	}
	w := e.Config.Workflows[0]
	w.Access = &config.Access{ChannelOnly: true, LocalServices: true, LocalFiles: true}
	// Old metadata deliberately has no current access proof. BeforeCreate would
	// fail immediately; a delivered receipt must not reach that guard or relay.
	for range 2 {
		recovered := &delivery.Publisher{MM: mm, BotID: mm.bot, Key: key}
		if err := recovered.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := e.publish(t.Context(), recovered, session, run, w); err != nil {
			t.Fatal("old receipt acquired a new export or publication", err)
		}
		if recovered.NoticeExists("attachments_failed", run.ID) || e.outputQueue[run.ID] || mm.calls != 1 {
			t.Fatal("replay changed a delivered run", mm.calls, e.outputQueue)
		}
	}
}

func TestFrozenLocalExportCannotBeDisabledByWorkflow(t *testing.T) {
	for _, metadataFlag := range []bool{false, true} {
		for _, currentFlag := range []bool{false, true} {
			e, _, mm, _, key := fixture(t)
			env := input("initial")
			env.LocalFiles = metadataFlag
			run := conversation.Run{ID: "run", Status: "completed", AgentStatus: "completed"}
			// The immutable session hook identifies already-admitted inputs that
			// predate the metadata flag, even if after_run has no result at all.
			session := conversation.Session{ID: "session", LocalFiles: !metadataFlag, Messages: []conversation.Message{{RunID: run.ID, Role: "user", Metadata: testMetadata(env)}}}
			w := e.Config.Workflows[0]
			w.Access = &config.Access{LocalServices: true, LocalFiles: currentFlag}
			p := &delivery.Publisher{MM: mm, BotID: mm.bot, Key: key}
			if err := e.publish(t.Context(), p, session, run, w); err == nil || !e.outputQueue[run.ID] || mm.calls != 0 {
				t.Fatal("incomplete frozen export bypassed the publication guard", metadataFlag, currentFlag, err)
			}
		}
	}
}
