package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	api "github.com/orpheus-agents/orpheus/client"
)

// projectAccess keeps source identities in the core's input authority and maps
// service identities only into the configured opaque provider fields.
func projectAccess(binding config.Access, audience []string, actor string) (api.AccessScopeContext, []api.AccessSubject, error) {
	if err := binding.Validate(); err != nil {
		return nil, nil, err
	}
	if len(audience) < 2 || len(audience) > 4096 || actor == binding.BotID {
		return nil, nil, errors.New("invalid protected audience or actor")
	}
	members := slices.Clone(audience)
	slices.Sort(members)
	allowed := make([]api.AccessSubject, 0, len(members))
	readers := make([]json.RawMessage, 0, len(members)-1)
	seen := make(map[string]bool, len(members))
	var missing []string
	for _, id := range members {
		if !config.ValidID(id) || seen[id] {
			return nil, nil, errors.New("invalid protected audience")
		}
		seen[id] = true
		if binding.Peer != nil && id == binding.Peer.ID {
			continue
		}
		allowed = append(allowed, api.AccessSubject{Namespace: binding.Namespace, ID: id})
		if id == binding.BotID {
			continue
		}
		mapped, ok := binding.IdentityProjection.Subjects[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		raw, err := mapped.MarshalJSON()
		if err != nil {
			return nil, nil, err
		}
		readers = append(readers, raw)
	}
	if !seen[actor] || !seen[binding.BotID] {
		return nil, nil, errors.New("protected actor or bot outside audience")
	}
	if peer := binding.Peer; peer != nil && (actor == peer.ID || !seen[peer.ID]) {
		return nil, nil, errors.New("protected peer or actor mismatch")
	}
	if len(missing) > 0 {
		return nil, nil, &audienceDenial{code: "identity_unmapped", ids: missing}
	}
	raw, err := json.Marshal(binding.ScopeContext)
	if err != nil {
		return nil, nil, err
	}
	var scope api.AccessScopeContext
	if err := json.Unmarshal(raw, &scope); err != nil {
		return nil, nil, err
	}
	if peer := binding.Peer; peer != nil {
		if actor == peer.ID || !seen[peer.ID] || len(audience) != len(binding.IdentityProjection.Subjects)+2 {
			return nil, nil, errors.New("protected peer or fixed audience mismatch")
		}
		if !binding.LocalProviderContext {
			scope[peer.Field], err = json.Marshal(peer.ID)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	actorID, err := binding.IdentityProjection.Subjects[actor].MarshalJSON()
	if err != nil {
		return nil, nil, err
	}
	scope[binding.IdentityProjection.ActorField] = bytes.Clone(actorID)
	scope[binding.IdentityProjection.AudienceField], err = json.Marshal(readers)
	return scope, allowed, err
}

func validateProtectedContext(env conversation.Envelope, w config.Workflow) error {
	a, proof := env.AccessContext, env.AccessProof
	if a == nil || proof == nil || a.Subject.Namespace != w.Access.Namespace || a.ContextObservedAt.IsZero() || a.AllowedInputSubjects == nil {
		return errors.New("missing protected authority")
	}
	digest, err := w.Access.Digest()
	if err != nil || digest != proof.ProjectionDigest {
		return errors.New("protected projection configuration changed")
	}
	scope, allowed, err := projectAccess(*w.Access, proof.AudienceSubjects, a.Subject.ID)
	if err != nil {
		return err
	}
	if len(allowed) != len(*a.AllowedInputSubjects) {
		return errors.New("protected input authority mismatch")
	}
	seen := make(map[api.AccessSubject]bool, len(allowed))
	for _, subject := range *a.AllowedInputSubjects {
		if seen[subject] || !slices.Contains(allowed, subject) {
			return errors.New("protected input authority mismatch")
		}
		seen[subject] = true
	}
	want, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(a.ScopeContext)
	if err != nil || !bytes.Equal(want, actual) {
		return errors.New("protected service projection mismatch")
	}
	return nil
}

func protectedContract(s *conversation.Session, r *conversation.Run, w config.Workflow, key conversation.Key) (conversation.Envelope, error) {
	if s == nil || r == nil {
		return conversation.Envelope{}, errors.New("missing protected run")
	}
	env, err := contract(*s, *r)
	if err != nil {
		return env, err
	}
	a := env.AccessContext
	if validateProtectedContext(env, w) != nil || env.ContextStart == nil || env.ContextStart.CreatedAt <= 0 || !config.ValidID(env.ContextStart.PostID) || env.Key() != key || a.InvocationID != key.Invocation(env.Anchor) {
		return env, errors.New("unknown protected context provenance")
	}
	return env, nil
}

// confirmAudience observes the channel, its membership and the active recipient types.
func (e *Engine) confirmAudience(ctx context.Context, w config.Workflow, key conversation.Key) ([]string, time.Time, error) {
	return e.readAudience(ctx, w, key, nil)
}

// readAudience optionally retains public usernames from the same verified membership scan.
func (e *Engine) readAudience(ctx context.Context, w config.Workflow, key conversation.Key, usernames map[string]string) ([]string, time.Time, error) {
	observed := time.Now().UTC()
	ch, err := e.MM.Channel(ctx, key.Channel)
	if err != nil {
		return nil, time.Time{}, err
	}
	if ch.ID != key.Channel || ch.Type != "P" || ch.DeleteAt != 0 || w.Access.BotID != e.Bot.ID || !slices.Contains(w.IncludeIDs, key.Channel) {
		return nil, time.Time{}, errors.New("protected channel binding mismatch")
	}
	audience, err := e.MM.Audience(ctx, key.Channel)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(audience) == 0 || !slices.Contains(audience, e.Bot.ID) {
		return nil, time.Time{}, errors.New("bot missing from audience")
	}
	// Confirm every recipient other than the authenticated execution bot.
	for _, id := range audience {
		if id == e.Bot.ID {
			continue
		}
		user, err := e.MM.User(ctx, id)
		if err != nil {
			return nil, time.Time{}, err
		}
		expectBot := w.Access.Peer != nil && id == w.Access.Peer.ID
		if user.ID != id || user.DeleteAt != 0 || user.IsBot != expectBot {
			return nil, time.Time{}, errors.New("unconfirmed audience identity")
		}
		if _, wanted := usernames[id]; wanted && !user.IsBot {
			usernames[id] = user.Username
		}
	}
	if time.Since(observed) >= time.Minute {
		return nil, time.Time{}, errors.New("audience observation expired")
	}
	return audience, observed, confirmAccountCards(ctx, w.Access, audience)
}

// Admission is called after the coordinator reserves a run slot. The result is
// frozen in the existing pending input, then in the core's accepted metadata.
func (e *Engine) admission(ctx context.Context, w config.Workflow, key conversation.Key, env conversation.Envelope, posts []mattermost.Post) (*api.AccessContext, *conversation.AccessProof, error) {
	audience, observed, err := e.confirmAudience(ctx, w, key)
	if err != nil {
		return nil, nil, err
	}
	var subject string
	for _, post := range posts {
		if !slices.Contains(env.TriggerIDs, post.ID) {
			continue
		}
		if !slices.Contains(audience, post.UserID) || (w.Access.Peer != nil && post.UserID == w.Access.Peer.ID) {
			return nil, nil, errors.New("trigger author outside audience")
		}
		if post.ID == env.Anchor {
			subject = post.UserID
		}
	}
	scope, allowed, err := projectAccess(*w.Access, audience, subject)
	if err != nil {
		return nil, nil, err
	}
	digest, err := w.Access.Digest()
	if err != nil {
		return nil, nil, err
	}
	proof := &conversation.AccessProof{AudienceSubjects: slices.Clone(audience), ProjectionDigest: digest}
	access := &api.AccessContext{InvocationID: key.Invocation(env.Anchor), Subject: api.AccessSubject{Namespace: w.Access.Namespace, ID: subject}, ScopeContext: scope, ContextObservedAt: observed, AllowedInputSubjects: &allowed}
	return access, proof, nil
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
	env.AccessProof = p.Envelope.AccessProof
	p.Envelope, p.Messages, p.Session = env, messages, ""
	return p, nil
}
