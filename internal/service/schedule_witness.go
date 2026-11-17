package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus/agentcontract"
	api "github.com/orpheus-agents/orpheus/client"
)

// The trusted schedule owner and occurrence are verified by the caller against
// the Space control plane. No Mattermost post, reaction or delivery is created.
type registeredOccurrence struct {
	Schedule, Occurrence                                                 uuid.UUID
	Slot                                                                 time.Time
	Source, Agent, Actor, Principal, ReadersDigest, Revision, Text, Chat string
}

func (e *Engine) corporateRoute(channel, kind string) (*config.Workflow, error) {
	e.corporateMu.RLock()
	defer e.corporateMu.RUnlock()
	cfg := e.Config
	if e.corporateConfig != nil {
		cfg = *e.corporateConfig
	}
	w, err := cfg.Route(channel, kind)
	if err != nil || w == nil {
		return nil, errors.New("no corporate route")
	}
	copy := *w
	return &copy, nil
}

func (e *Engine) scheduleWitness(ctx context.Context, p registeredOccurrence, now time.Time) (*api.AccessContext, error) {
	begin := time.Now()
	denied := errors.New("schedule audience unconfirmed")
	if p.Schedule == uuid.Nil || p.Occurrence == uuid.Nil || p.Occurrence == p.Schedule || p.Slot.Nanosecond() != 0 || p.Slot.Unix()%int64((5*time.Minute)/time.Second) != 0 || now.Before(p.Slot) || !now.Before(p.Slot.Add(5*time.Minute)) || p.Source != e.SourceID || !config.ValidID(p.Chat) || !config.ValidID(p.Actor) || p.Agent == "" || p.Text == "" {
		return nil, denied
	}
	thread, message := "space-schedule:"+p.Schedule.String(), "space-occurrence:"+p.Occurrence.String()
	ctx = scheduleTrace(ctx, thread, message)
	ch, err := e.MM.Channel(ctx, p.Chat)
	if err != nil || ch.ID != p.Chat {
		return nil, denied
	}
	w, err := e.corporateRoute(p.Chat, ch.Type)
	if err != nil || w.Draining || w.AgentID != p.Agent || w.Access == nil || !w.Access.VerifyAccountCards || w.EffectiveRevision != p.Revision || !slices.Contains(w.Services, "kord") {
		return nil, denied
	}
	key := conversation.Key{Source: e.SourceID, Workflow: w.ID, Channel: p.Chat, Root: thread}
	roster, observed, err := e.confirmAudience(ctx, *w, key)
	if err != nil {
		return nil, denied
	}
	scope, allowed, err := projectAccess(*w.Access, roster, p.Actor)
	if err != nil {
		return nil, denied
	}
	var readers []int64
	if json.Unmarshal(scope[w.Access.IdentityProjection.AudienceField], &readers) != nil || len(readers) == 0 {
		return nil, denied
	}
	slices.Sort(readers)
	payload, err := json.Marshal(readers)
	if err != nil {
		return nil, denied
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != p.ReadersDigest {
		return nil, denied
	}
	descriptor, err := e.describeAudience(ctx, *w, key, roster)
	if err != nil {
		return nil, denied
	}
	a := &api.AccessContext{InvocationID: p.Occurrence, Subject: api.AccessSubject{Namespace: w.Access.Namespace, ID: p.Actor}, ScopeContext: scope, ContextObservedAt: observed, AllowedInputSubjects: &allowed}
	route := agentcontract.Route{Connector: "mattermost", Source: e.SourceID, Chat: p.Chat, Thread: thread, Message: message}
	if err := scheduleAssignment(*w, a, descriptor, roster, route, p.Text); err != nil {
		return nil, denied
	}
	task, err := agentcontract.Decode(*a.Assignment)
	if err != nil || task.Author.PrincipalID != p.Principal || task.Author.ID != p.Actor || task.Author.BotID != "" || task.InvocationID != p.Occurrence {
		return nil, denied
	}
	if err = e.confirmCorporate(ctx, task); err != nil {
		return nil, denied
	}
	if time.Since(observed) >= time.Minute || !now.Add(time.Since(begin)).Before(p.Slot.Add(5*time.Minute)) {
		return nil, denied
	}
	return a, nil
}

// scheduleAssignment seals the same corporate-assignment.v1 as an ordinary
// message, for a job that has no Mattermost post.
func scheduleAssignment(w config.Workflow, a *api.AccessContext, audience agentcontract.Audience, roster []string, route agentcontract.Route, text string) error {
	readers, err := corporateReaders(w, roster)
	if err != nil {
		return err
	}
	var author agentcontract.Principal
	for _, r := range readers {
		if r.ID == a.Subject.ID {
			author = r
		}
	}
	task := agentcontract.Assignment{Audience: audience, ProviderContext: a.ScopeContext, Schema: agentcontract.Version, InvocationID: a.InvocationID, Agent: w.AgentID, Author: author, Readers: readers, Text: text, Delivery: route, Confirmation: agentcontract.Confirmation{Revision: w.EffectiveRevision, ObservedAt: a.ContextObservedAt}}
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

// The only inputs from the requester are two opaque references. The author,
// channel, current slot and readers must come from an independent registrar.
func privateScheduleHandler(check func(*http.Request) (registeredOccurrence, error), lookup func(string) *Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, err := check(r)
		if err != nil {
			http.Error(w, "schedule unconfirmed", http.StatusForbidden)
			return
		}
		e := lookup(p.Source)
		if e == nil {
			http.Error(w, "schedule unconfirmed", http.StatusForbidden)
			return
		}
		a, err := e.scheduleWitness(r.Context(), p, time.Now())
		if err != nil {
			http.Error(w, "schedule unconfirmed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a)
	})
}
