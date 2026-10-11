package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	api "github.com/orpheus-agents/orpheus/client"
)

func protectedContract(s *conversation.Session, r *conversation.Run, w config.Workflow, key conversation.Key) (conversation.Envelope, error) {
	if s == nil || r == nil {
		return conversation.Envelope{}, errors.New("missing protected run")
	}
	env, err := contract(*s, *r)
	if err != nil {
		return env, err
	}
	a := env.AccessContext
	if a == nil || env.ContextStart == nil || env.ContextStart.CreatedAt <= 0 || !config.ValidID(env.ContextStart.PostID) || env.Key() != key || a.InvocationID != key.Invocation(env.Anchor) || a.Subject.Namespace != w.Access.Namespace || a.ScopeContext.ChannelID != key.Channel || a.ScopeContext.AgentID != w.Access.AgentID || a.ScopeContext.PolicySpaceID != w.Access.PolicySpaceID || !slices.Contains(a.ScopeContext.AudienceSubjects, w.Access.BotID) {
		return env, errors.New("unknown protected context provenance")
	}
	return env, nil
}

// Admission is called after the coordinator reserves a run slot. The result is
// frozen in the existing pending input, then in the core's accepted metadata.
func (e *Engine) admission(ctx context.Context, w config.Workflow, key conversation.Key, env conversation.Envelope, posts []mattermost.Post) (*api.AccessContext, error) {
	observed := time.Now().UTC()
	ch, err := e.MM.Channel(ctx, key.Channel)
	if err != nil {
		return nil, err
	}
	if ch.ID != key.Channel || ch.Type != "P" || ch.DeleteAt != 0 || w.Access.BotID != e.Bot.ID || !slices.Contains(w.IncludeIDs, key.Channel) {
		return nil, errors.New("protected channel binding mismatch")
	}
	audience, err := e.MM.Audience(ctx, key.Channel)
	if err != nil {
		return nil, err
	}
	if len(audience) == 0 || !slices.Contains(audience, e.Bot.ID) {
		return nil, errors.New("bot missing from audience")
	}
	var subject string
	for _, post := range posts {
		if !slices.Contains(env.TriggerIDs, post.ID) {
			continue
		}
		if !slices.Contains(audience, post.UserID) {
			return nil, errors.New("trigger author outside audience")
		}
		if post.ID == env.Anchor {
			subject = post.UserID
		}
	}
	author, err := e.MM.User(ctx, subject)
	if err != nil {
		return nil, err
	}
	if subject == "" || author.ID != subject || author.DeleteAt != 0 {
		return nil, errors.New("unconfirmed trigger author")
	}
	if time.Since(observed) >= time.Minute {
		return nil, errors.New("audience observation expired")
	}
	return &api.AccessContext{InvocationID: key.Invocation(env.Anchor), Subject: api.AccessSubject{Namespace: w.Access.Namespace, ID: subject}, ScopeContext: api.AccessScopeContext{AudienceSubjects: slices.Clone(audience), ChannelID: key.Channel, AgentID: w.Access.AgentID, PolicySpaceID: w.Access.PolicySpaceID}, ContextObservedAt: observed}, nil
}

func (e *Engine) cleanPending(ctx context.Context, p pending) (pending, error) {
	// Only frozen trigger posts enter this builder. Do not transfer an old payload,
	// a previous file index, or context from the previous sandbox.
	builder := conversation.Builder{Source: e.MM, Config: e.Config, Workflow: p.Workflow, Bot: e.Bot, TriggerOnly: true}
	env, messages, err := builder.BuildMessages(ctx, p.Envelope.Key(), p.Channel, p.TriggerPosts, conversation.Snapshot{}, true, time.Now())
	if err != nil {
		return p, err
	}
	if env.Anchor != p.Envelope.Anchor || !slices.Equal(env.TriggerIDs, p.Envelope.TriggerIDs) {
		return p, errors.New("clean input changed its frozen triggers")
	}
	env.AccessContext = p.Envelope.AccessContext
	p.Envelope, p.Messages, p.Session = env, messages, ""
	return p, nil
}
