package delivery

import (
	"context"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

// CleanupProgress runs only after the caller has confirmed every final part.
// Receipts bind deletion to this bot, source, workflow, thread, session and run.
// A changed post, attached file, user post or another run can never be selected.
func (p *Publisher) CleanupProgress(ctx context.Context, session, run string) error {
	source, ok := p.MM.(interface {
		Delete(context.Context, string) error
	})
	if !ok {
		return nil
	}
	for i, post := range p.Posts {
		r, valid := p.receipt(post)
		if !valid || !r.Temporary || r.SessionID != session || r.RunID != run || len(post.FileIDs) != 0 || len(r.Attachments) != 0 || r.Hash != attachments.Hash([]byte(post.Message)) {
			continue
		}
		if err := source.Delete(ctx, post.ID); err != nil && mattermost.Status(err) != 404 {
			return err
		}
		p.Posts[i].DeleteAt = 1
	}
	return nil
}

// Once any final part is persisted, later rate changes or late usage reports
// must not change its content or break immutable delivery receipts.
func (p *Publisher) FrozenCost(session, run string) string {
	for _, post := range p.Posts {
		r, ok := p.receipt(post)
		if ok && !r.Temporary && r.SessionID == session && r.RunID == run && r.CostFooter != "" && r.Hash == attachments.Hash([]byte(post.Message)) {
			return r.CostFooter
		}
	}
	return ""
}
