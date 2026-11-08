package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

func corporateReaders(w config.Workflow, roster []string) ([]agentcontract.Principal, error) {
	var readers []agentcontract.Principal
	if w.Access == nil {
		return nil, errors.New("corporate audience missing")
	}
	for _, id := range roster {
		if id == w.Access.BotID {
			continue
		}
		mapped, ok := w.Access.IdentityProjection.Subjects[id]
		botID := ""
		if peer := w.Access.Peer; peer != nil && peer.ID == id && len(peer.BotID) > 0 && len(peer.OwnerUserID) > 0 {
			mapped, ok = peer.OwnerUserID, true
			rawBot, err := peer.BotID.MarshalJSON()
			if err != nil {
				return nil, err
			}
			botID = strings.Trim(string(rawBot), "\"")
		}
		if !ok {
			return nil, errors.New("unmapped reader")
		}
		raw, err := mapped.MarshalJSON()
		if err != nil {
			return nil, err
		}
		readers = append(readers, agentcontract.Principal{BotID: botID, Namespace: w.Access.Namespace, ID: id, PrincipalID: strings.Trim(string(raw), "\"")})
	}
	return readers, nil
}
func corporateAssignment(p *pending) error {
	w := p.Workflow
	if w.AgentID == "" {
		return nil
	}
	a, proof := p.Envelope.AccessContext, p.Envelope.AccessProof
	if a == nil || proof == nil || proof.Audience == nil || !w.Access.VerifyAccountCards {
		return errors.New("corporate agent requires confirmed employee cards")
	}
	readers, err := corporateReaders(w, proof.AudienceSubjects)
	if err != nil {
		return err
	}
	var author agentcontract.Principal
	for _, r := range readers {
		if r.ID == a.Subject.ID {
			author = r
		}
	}
	var text []string
	for _, m := range p.Messages {
		text = append(text, m.Text)
	}
	task := agentcontract.Assignment{Audience: *proof.Audience, ProviderContext: a.ScopeContext, Schema: agentcontract.Version, InvocationID: a.InvocationID, Agent: w.AgentID, Author: author, Readers: readers, Text: strings.Join(text, "\n\n"), Delivery: agentcontract.Route{Connector: "mattermost", Source: p.Envelope.Source, Chat: p.Envelope.Channel, Thread: p.Envelope.Root, Message: p.Envelope.Anchor}, Confirmation: agentcontract.Confirmation{Revision: w.EffectiveRevision, ObservedAt: a.ContextObservedAt}}
	for _, f := range p.Envelope.Request.Files {
		task.Attachments = append(task.Attachments, agentcontract.Attachment{ID: f.FileID, Name: f.Name})
	}
	if err = task.Seal(os.Getenv("ORPHEUS_AUDIENCE_SECRET")); err != nil {
		return err
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	a.Assignment = new(json.RawMessage(raw))
	return nil
}
func (e *Engine) confirmCorporate(ctx context.Context, a agentcontract.Assignment) error {
	denied := errors.New("audience unconfirmed")
	if a.Delivery.Connector != "mattermost" || a.Delivery.Source != e.SourceID {
		return denied
	}
	e.corporateMu.RLock()
	cfg := e.Config
	if e.corporateConfig != nil {
		cfg = *e.corporateConfig
	}
	w, err := cfg.Route(a.Delivery.Chat, a.Audience.ChannelType)
	e.corporateMu.RUnlock()
	if err != nil || w == nil || w.Draining || w.AgentID != a.Agent || w.Access == nil || !w.Access.VerifyAccountCards {
		return denied
	}
	key := conversation.Key{Source: e.SourceID, Workflow: w.ID, Channel: a.Delivery.Chat, Root: a.Delivery.Thread}
	if w.EffectiveRevision != a.Confirmation.Revision {
		return denied
	}
	roster, _, err := e.confirmAudience(ctx, *w, key)
	if err != nil {
		return denied
	}
	readers, err := corporateReaders(*w, roster)
	if err != nil {
		return denied
	}
	copy := a
	copy.Readers = readers
	descriptor, err := e.describeAudience(ctx, *w, key, roster)
	if err != nil {
		return denied
	}
	copy.Audience = descriptor
	if copy.Digest() != a.Digest() {
		return denied
	}
	return nil
}
func (e *Engine) corporateHandler() http.Handler {
	return agentcontract.Handler(func() string { return os.Getenv("ORPHEUS_AUDIENCE_SECRET") }, e.confirmCorporate)
}

func (e *Engine) setCorporateConfig(cfg config.Config) {
	e.corporateMu.Lock()
	defer e.corporateMu.Unlock()
	e.corporateConfig = &cfg
}
