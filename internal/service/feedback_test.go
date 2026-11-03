package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	api "github.com/orpheus-agents/orpheus/client"
)

type feedbackMM struct {
	*fakeSource
	loseFinal bool
	deleted   []string
}

func (f *feedbackMM) Create(ctx context.Context, p mattermost.CreatePost) (mattermost.Post, error) {
	post, err := f.fakeSource.Create(ctx, p)
	if f.loseFinal && strings.Contains(p.Message, "FINAL") {
		f.loseFinal = false
		return mattermost.Post{}, errors.New("lost final acknowledgement")
	}
	return post, err
}
func (f *feedbackMM) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	for i := range f.posts {
		if f.posts[i].ID == id {
			f.posts[i].DeleteAt = 1
		}
	}
	return nil
}

func TestFeedbackFinalLossAndRestart(t *testing.T) {
	e, core, base, _, key := fixture(t)
	w := e.Config.Workflows[0]
	if err := e.Thread(t.Context(), w, key, mattermost.Channel{Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	s := core.snapshot.Sessions[0]
	r := s.Runs[0]
	r.CreatedAt = time.Now()
	s.Model = "sol"
	t.Setenv("BOT_FEEDBACK_SINCE", "1")
	path := filepath.Join(t.TempDir(), "costs.json")
	data := `{"fx":{"` + r.CreatedAt.In(time.FixedZone("MSK", 10800)).Format(time.DateOnly) + `":80},"models":{"sol":{"input":2,"cached":0.1,"output":10}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOT_COST_FILE", path)
	mm := &feedbackMM{fakeSource: base}
	e.MM = mm
	publisher := func() *delivery.Publisher {
		p := &delivery.Publisher{MM: mm, Key: key, BotID: base.bot}
		if err := p.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := e.publish(t.Context(), publisher(), s, r, w); err != nil {
		t.Fatal(err)
	}
	if len(mm.deleted) != 0 {
		t.Fatal("deleted before final")
	}
	r.Status = "completed"
	r.FinalMessageID = "final"
	r.Usage = api.Usage{InputTokens: 1000, TotalTokens: 1000}
	s.Messages = append(s.Messages, conversation.Message{ID: "final", RunID: r.ID, Role: "assistant", Kind: "answer", Text: "FINAL", Position: 1})
	// No agent status means no output hook is required for this test run.
	mm.loseFinal = true
	if err := e.publish(t.Context(), publisher(), s, r, w); err == nil {
		t.Fatal("lost ack ignored")
	}
	if len(mm.deleted) != 0 {
		t.Fatal("deleted without receipt")
	}
	// A fresh publisher reconciles the final receipt and preserves progress.
	if err := e.publish(t.Context(), publisher(), s, r, w); err != nil {
		t.Fatal(err)
	}
	if len(mm.deleted) != 0 {
		t.Fatal(mm.deleted)
	}
	finalCount, progressCount := 0, 0
	for _, p := range mm.posts {
		var receipt delivery.Receipt
		if p.DeleteAt != 0 {
			t.Fatal("feedback was deleted", p.ID)
		}
		if json.Unmarshal(p.Props[delivery.Property], &receipt) == nil && receipt.MessageID == "feedback_started" {
			progressCount++
		}
		if json.Unmarshal(p.Props[delivery.Property], &receipt) == nil && receipt.MessageID == "final" {
			finalCount++
			if !strings.Contains(p.Message, "Стоимость обработки: ≈0,16 ₽") {
				t.Fatal(p.Message)
			}
		}
	}
	if progressCount != 1 {
		t.Fatal("missing or duplicate progress", progressCount)
	}
	if finalCount != 1 {
		t.Fatal("duplicate final", finalCount)
	}
}

// Every terminal outcome must preserve already delivered progress. In
// Mattermost, deleting it produces visible "message deleted" placeholders.
func TestFeedbackPreservedForEveryTerminalOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		for _, commentary := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/commentary=%t", status, commentary), func(t *testing.T) {
				e, core, base, _, key := fixture(t)
				w := e.Config.Workflows[0]
				w.SendCommentary = new(commentary)
				if err := e.Thread(t.Context(), w, key, mattermost.Channel{Type: "O"}, true); err != nil {
					t.Fatal(err)
				}
				s := core.snapshot.Sessions[0]
				r := s.Runs[0]
				r.CreatedAt = time.Now()
				t.Setenv("BOT_FEEDBACK_SINCE", "1")
				s.Messages = append(s.Messages, conversation.Message{ID: "progress", RunID: r.ID, Role: "assistant", Kind: "progress", Text: "Проверяю данные.", Position: 1})
				mm := &feedbackMM{fakeSource: base}
				e.MM = mm
				publish := func() {
					t.Helper()
					p := &delivery.Publisher{MM: mm, Key: key, BotID: base.bot}
					if err := p.Refresh(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err := e.publish(t.Context(), p, s, r, w); err != nil {
						t.Fatal(err)
					}
				}
				publish()
				// Reconciliation while running must not duplicate either progress post.
				publish()
				r.Status = status
				if status == "completed" {
					r.FinalMessageID = "final"
					s.Messages = append(s.Messages, conversation.Message{ID: "final", RunID: r.ID, Role: "assistant", Kind: "answer", Text: "FINAL", Position: 2})
				}
				publish()
				// Restart/replay must preserve and reuse all existing delivery receipts.
				publish()
				if len(mm.deleted) != 0 {
					t.Fatal("deleted feedback", mm.deleted)
				}
				counts := map[string]int{}
				for _, post := range mm.posts {
					if post.DeleteAt != 0 {
						t.Fatal("deleted post", post.ID)
					}
					var receipt delivery.Receipt
					if json.Unmarshal(post.Props[delivery.Property], &receipt) == nil && receipt.RunID == r.ID {
						counts[receipt.MessageID]++
					}
				}
				final := "run_status"
				if status == "completed" {
					final = "final"
				}
				for _, id := range []string{"feedback_started", "progress", final} {
					if counts[id] != 1 {
						t.Fatalf("%s: got %d posts, want 1", id, counts[id])
					}
				}
			})
		}
	}
}
