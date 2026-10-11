package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type CheckpointAPI interface {
	Sessions(context.Context, string, string) ([]conversation.Session, error)
	Snapshot(context.Context, conversation.Key) (conversation.Snapshot, error)
}
type CheckpointSource interface {
	Me(context.Context) (mattermost.User, error)
	Posts(context.Context, string, time.Time) ([]mattermost.Post, error)
	Thread(context.Context, string) ([]mattermost.Post, error)
	File(context.Context, string) (mattermost.FileInfo, error)
}

// ExportCheckpoint reads full paginated API history and persisted MM receipts.
// The caller must independently stop all writers and resolve uncertain HTTP
// admissions. Repeated reads cannot prove absence of an in-flight remote write.
func ExportCheckpoint(ctx context.Context, w config.Workflow, api CheckpointAPI, mm CheckpointSource, epoch string) (config.AdmissionCheckpoint, error) {
	fail := func(err error) (config.AdmissionCheckpoint, error) { return config.AdmissionCheckpoint{}, err }
	id, err := uuid.Parse(epoch)
	if err != nil || id == uuid.Nil || id.String() != epoch || w.AdmissionCheckpoint == nil || epoch == w.AdmissionCheckpoint.Epoch || !w.Draining {
		return fail(errors.New("export requires a drained checkpoint workflow and new epoch"))
	}
	if err = w.ValidateCheckpoint(); err != nil {
		return fail(err)
	}
	bot, err := mm.Me(ctx)
	if err != nil {
		return fail(err)
	}
	cp := *w.AdmissionCheckpoint
	if bot.ID != cp.BotID {
		return fail(errors.New("checkpoint bot mismatch"))
	}
	source := w.Mattermost.Source(bot.ID)
	key := conversation.Key{Source: source, Workflow: w.ID, Channel: cp.ChannelID}
	sessions, err := api.Sessions(ctx, key.Namespace(), "")
	if err != nil {
		return fail(err)
	}
	roots := map[string]bool{}
	listed := map[string]string{}
	for _, s := range sessions {
		k, err := conversation.ParseKey(source, w.ID, s.ExternalKey)
		if err != nil {
			return fail(err)
		}
		if k.Channel != cp.ChannelID {
			continue
		}
		if s.ID == "" || listed[s.ID] != "" {
			return fail(errors.New("invalid session listing"))
		}
		roots[k.Root] = true
		listed[s.ID] = k.Root
	}
	posts, err := mm.Posts(ctx, cp.ChannelID, time.UnixMilli(cp.ReplayFromMS))
	if err != nil {
		return fail(err)
	}
	for _, p := range posts {
		if p.ChannelID != cp.ChannelID || !config.ValidID(p.Root()) {
			return fail(errors.New("invalid channel replay"))
		}
		if _, has := p.Props[delivery.Property]; has && p.UserID == bot.ID {
			roots[p.Root()] = true
		}
	}
	consumed := map[string]bool{}
	for _, id := range cp.ConsumedPostIDs {
		consumed[id] = true
	}
	for _, root := range slices.Sorted(maps.Keys(roots)) {
		key.Root = root
		snap, err := api.Snapshot(ctx, key)
		if err != nil {
			return fail(err)
		}
		thread, err := mm.Thread(ctx, root)
		if err != nil {
			return fail(err)
		}
		audit, err := delivery.NewAudit(thread, key, bot.ID)
		if err != nil {
			return fail(err)
		}
		for _, s := range snap.Sessions {
			if listed[s.ID] != root || s.ExternalKey != key.External() {
				return fail(errors.New("session changed during export"))
			}
			delete(listed, s.ID)
			runs := map[string]bool{}
			for _, r := range s.Runs {
				if r.ID == "" || runs[r.ID] || r.SessionID != s.ID || !r.Terminal() {
					return fail(errors.New("unfinished or invalid run"))
				}
				runs[r.ID] = true
				if err = verifyRun(ctx, audit, mm, key, bot.ID, s, r); err != nil {
					return fail(err)
				}
			}
			for _, m := range s.Messages {
				if !runs[m.RunID] {
					return fail(errors.New("message lacks run"))
				}
				if m.Role != "user" {
					continue
				}
				if m.Delivery != "delivered" {
					return fail(errors.New("unresolved input delivery"))
				}
				// Multi-message batches carry the envelope only on their final item.
				// Snapshot validates that each batch is closed by an envelope.
				if !conversation.HasMetadata(m.Metadata) {
					continue
				}
				var env conversation.Envelope
				dec := json.NewDecoder(bytes.NewReader(m.Metadata))
				dec.DisallowUnknownFields()
				if err = dec.Decode(&env); err != nil {
					return fail(errors.New("unknown input metadata"))
				}
				env, err = conversation.Decode(m.Metadata)
				if err != nil || env.Key() != key || m.ExternalKey != key.MessageKey(env.Anchor) {
					return fail(errors.New("invalid input metadata"))
				}
				for _, id := range env.TriggerIDs {
					if !config.ValidID(id) {
						return fail(errors.New("invalid consumed ID"))
					}
					consumed[id] = true
				}
			}
		}
		ids, err := audit.Rejections()
		if err != nil {
			return fail(err)
		}
		for _, id := range ids {
			consumed[id] = true
		}
		if err = audit.Complete(); err != nil {
			return fail(err)
		}
	}
	if len(listed) > 0 {
		return fail(errors.New("session disappeared during export"))
	}
	if len(consumed) > 10000 {
		return fail(errors.New("checkpoint too large"))
	}
	cp.Epoch = epoch
	cp.ConsumedPostIDs = slices.Sorted(maps.Keys(consumed))
	// Keep the original inclusive replay boundary. Advancing to the newest post
	// would skip unaccepted mentions left by draining or an interrupted batch.
	return cp, nil
}

func verifyRun(ctx context.Context, a *delivery.Audit, mm CheckpointSource, key conversation.Key, bot string, s conversation.Session, r conversation.Run) error {
	env, err := contract(s, r)
	if err != nil {
		return err
	}
	send := func(mid, text string, files []attachments.Artifact) error {
		for _, part := range delivery.Parts(key, s.ID, r.ID, mid, text, env.Render, files) {
			if err := a.Verify(ctx, mm, part); err != nil {
				return err
			}
		}
		return nil
	}
	stopped := a.HasNotice(s.ID, r.ID, "attachments_failed")
	var output attachments.Output
	if stopped {
		if err = send("attachments_failed", "Could not deliver the result attachments.", nil); err != nil {
			return err
		}
	} else {
		output, err = hookOutput(s, r, env, bot)
		if err != nil {
			return err
		}
	}
	if r.FinalMessageID != "" {
		count := 0
		for _, m := range s.Messages {
			if m.RunID == r.ID && m.ID == r.FinalMessageID && m.Role == "assistant" && m.Kind == "answer" && m.Position >= 0 {
				count++
			}
		}
		if count != 1 {
			return errors.New("final message missing or ambiguous")
		}
	}
	hasAnswer := false
	for _, m := range s.Messages {
		if m.RunID != r.ID || m.Role != "assistant" || m.Position < 0 {
			continue
		}
		if m.Kind == "progress" {
			if env.Render.Commentary && m.Text != "" {
				if err = send(m.ID, m.Text, nil); err != nil {
					return err
				}
			}
			continue
		}
		if m.Kind != "answer" {
			return errors.New("unknown assistant message kind")
		}
		var files []attachments.Artifact
		if m.ID == r.FinalMessageID && !stopped {
			files = output.Files
		}
		if m.Text == "" && len(files) == 0 {
			continue
		}
		hasAnswer = true
		if err = send(m.ID, m.Text, files); err != nil {
			return err
		}
	}
	if r.FinalMessageID == "" && len(output.Files) > 0 {
		if err = send("attachments_only", "", output.Files); err != nil {
			return err
		}
		hasAnswer = true
	}
	if text := delivery.Failure(r, env.Render); text != "" {
		return send("run_status", text, nil)
	}
	if !hasAnswer && !stopped {
		return send("empty_result", "Run finished without a text answer or files.", nil)
	}
	return nil
}
