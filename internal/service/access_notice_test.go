package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

func TestMappingNoticeInThreadWithoutGrants(t *testing.T) {
	e, a, mm, box, key, w := protectedFixture(t)
	e.Config.AccessNoticesFrom = time.UnixMilli(1000)
	e.Config.AccessNoticeService = "Kord"
	actor := mm.posts[0].UserID
	const other = "zzzzzzzzzzzzzzzzzzzzzzzzzz"
	delete(w.Access.IdentityProjection.Subjects, actor)
	mm.audience = append(mm.audience, other)
	mm.users = map[string]mattermost.User{actor: {ID: actor, Username: "alex", Email: "private@example.invalid"}, other: {ID: other, Username: "lee"}}
	for range 2 {
		if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 1 || len(a.attempts) != 0 || box.prepared != 0 {
		t.Fatal("notice did not stop admission or delivery was repeated")
	}
	note := mm.posts[len(mm.posts)-1]
	if note.ChannelID != key.Channel || note.RootID != key.Root || !strings.Contains(note.Message, "`alex`") || !strings.Contains(note.Message, "`lee`") || !strings.Contains(note.Message, "Kord") {
		t.Fatal("missing reader list or original thread")
	}
	b, _ := json.Marshal(note)
	for _, secret := range []string{"ORIGINAL_REQUEST_BODY", "private@example.invalid", actor, other} {
		if strings.Contains(string(b), secret) {
			t.Fatal("notice exposes an unrelated source field")
		}
	}
	p := delivery.Publisher{MM: mm, Key: key, BotID: e.Bot.ID}
	if err := p.Refresh(t.Context()); err != nil || !p.Rejected(w.EffectiveRevision)[key.Root] {
		t.Fatal("rejection missing after refresh")
	}
	w.Access.IdentityProjection.Subjects[actor] = config.SubjectID(`42`)
	w.Access.IdentityProjection.Subjects[other] = config.SubjectID(`98`)
	w.EffectiveRevision = "new"
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil || len(a.attempts) != 0 {
		t.Fatal("denied input replayed after mapping correction", err)
	}
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", actor, "@orpheus repeat", 4000)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil || len(a.attempts) != 1 || a.attempts[0].Envelope.Anchor == key.Root {
		t.Fatal("fresh request after mapping correction failed", err)
	}
}

func TestMappingNoticeCutover(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	e.Config.AccessNoticesFrom = time.UnixMilli(2000)
	delete(w.Access.IdentityProjection.Subjects, mm.posts[0].UserID)
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil || mm.calls != 0 {
		t.Fatal("historical denial generated a notification")
	}
	appendAccessPost(mm, key, "ssssssssssssssssssssssssss", mm.posts[0].UserID, "@orpheus repeat", 3000)
	for range 2 {
		if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 1 || len(a.attempts) != 0 {
		t.Fatal("new mention in old thread must get one notice")
	}
}

type changingAudience struct {
	*accessSource
	changeAt int
}

func (m *changingAudience) Audience(ctx context.Context, channel string) ([]string, error) {
	list, err := m.accessSource.Audience(ctx, channel)
	if m.reads >= m.changeAt {
		return list[:2], err
	}
	return list, err
}
func TestMappingNoticeConfirmsAudienceBeforeDelivery(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	e.Config.AccessNoticesFrom = time.UnixMilli(1000)
	e.Config.AccessNoticeService = "Kord"
	mm.audience = append(mm.audience, "zzzzzzzzzzzzzzzzzzzzzzzzzz")
	e.MM = &changingAudience{mm, 3}
	err := e.Thread(t.Context(), w, key, mm.channel, true)
	if !errors.Is(err, errNoticeChanged) || mm.calls != 0 || len(a.attempts) != 0 {
		t.Fatal("changed roster was used for publication", err)
	}
}
func TestNoticeReasonUsesOnlyChannelNames(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	actor := mm.posts[0].UserID
	mm.users = map[string]mattermost.User{actor: {ID: actor, Username: "@channel\n[bad](https://example.invalid)", Nickname: "secret", Email: "private@example.invalid"}}
	for _, code := range []string{"identity_unconfirmed", "identity_unmapped", "participant_access_unconfirmed"} {
		b, _ := json.Marshal(map[string]any{"code": code, "participant_ids": []string{actor}})
		d, ok := errors.AsType[*audienceDenial](policyReason(403, strings.NewReader(string(b)), mm.audience, w.Access))
		if !ok || d.code != code || !slices.Equal(d.ids, []string{actor}) {
			t.Fatal("typed refusal lost")
		}
		text, err := denialText(d, map[string]string{actor: mm.users[actor].Username}, "Kord")
		if err != nil || !strings.Contains(text, "без доступного имени") || strings.Contains(text, "@") || strings.Contains(text, actor) || strings.Contains(text, "secret") {
			t.Fatal("untrusted profile exposed", err)
		}
	}
	for _, raw := range []string{`{"code":"identity_unconfirmed","participant_ids":["foreign"]}`, `{"code":"identity_unconfirmed","participant_ids":[]}`, `not JSON`, strings.Repeat("z", 129<<10)} {
		d, _ := errors.AsType[*audienceDenial](policyReason(503, strings.NewReader(raw), mm.audience, w.Access))
		if d.code != "channel_policy_unavailable" || len(d.ids) != 0 {
			t.Fatal("unconfirmed identity exposed")
		}
	}
}
func TestBlockedResultPostsControlNoticeOnly(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	e.Config.AccessNoticesFrom = time.UnixMilli(1000)
	e.Config.AccessNoticeService = "Kord"
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	s := &a.snapshot.Sessions[0]
	r := &s.Runs[0]
	r.Status, r.FinalMessageID = "completed", "answer"
	s.Messages = append(s.Messages, conversation.Message{ID: "answer", RunID: r.ID, Role: "assistant", Kind: "answer", Position: 1, Text: "private answer"})
	mm.audience = append(mm.audience, "zzzzzzzzzzzzzzzzzzzzzzzzzz")
	for range 2 {
		if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 1 || strings.Contains(mm.posts[len(mm.posts)-1].Message, "private answer") {
		t.Fatal("protected result released or notification repeated")
	}
	mm.audience = mm.audience[:2]
	if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 2 || !strings.Contains(mm.posts[len(mm.posts)-1].Message, "private answer") {
		t.Fatal("approved original audience did not receive its result")
	}
}

func TestCoreRefusalDoesNotIdentifyParticipants(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	a.failure = &orpheus.Error{Status: 403, Code: "service_access_denied"}
	for range 2 {
		if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if mm.calls != 1 || len(a.attempts) != 1 || len(a.snapshot.Sessions) != 0 {
		t.Fatal("refused request duplicated")
	}
	text := mm.posts[len(mm.posts)-1].Message
	if !strings.Contains(text, "Служба не указала, для какого участника") || strings.Contains(text, "human") || strings.Contains(text, "service_access_denied") {
		t.Fatal("invented unmapped identity or leaked provider detail")
	}
}

type lostNoticeResponse struct {
	*accessSource
	lost bool
}

func (m *lostNoticeResponse) Create(ctx context.Context, post mattermost.CreatePost) (mattermost.Post, error) {
	result, err := m.accessSource.Create(ctx, post)
	if !m.lost {
		m.lost = true
		return mattermost.Post{}, errors.New("receipt not returned")
	}
	return result, err
}
func TestNoticeResumesAfterLostFirstPart(t *testing.T) {
	e, a, mm, _, key, w := protectedFixture(t)
	w.MaxPostChars = 64
	mm.audience = append(mm.audience, "zzzzzzzzzzzzzzzzzzzzzzzzzz")
	e.MM = &lostNoticeResponse{accessSource: mm}
	_ = e.Thread(t.Context(), w, key, mm.channel, true)
	// The next receiver reads persisted posts only, including a part created before the lost response.
	e.pending = nil
	e.MM = mm
	for range 3 {
		if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	parts := map[int]bool{}
	count := 0
	for _, post := range mm.posts {
		if post.UserID != mm.bot {
			continue
		}
		var rec delivery.Receipt
		if json.Unmarshal(post.Props[delivery.Property], &rec) != nil || rec.MessageID != "input_unmapped" || parts[rec.Part] {
			t.Fatal("missing or duplicated fragment")
		}
		parts[rec.Part] = true
		count = rec.Parts
	}
	if count < 2 || len(parts) != count || len(a.attempts) != 0 {
		t.Fatal("notice was not recovered in full")
	}
}
