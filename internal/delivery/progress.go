package delivery

import (
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
)

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
