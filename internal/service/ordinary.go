package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

// Field order is also the canonical JSON order used by the provider's observation digest.
type ordinaryRecipient struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type ordinaryObservation struct {
	Actor       string              `json:"actor"`
	Complete    bool                `json:"complete"`
	Destination string              `json:"destination"`
	ObservedAt  int64               `json:"observed_at"`
	Recipients  []ordinaryRecipient `json:"recipients"`
	Source      string              `json:"source"`
}

func (o ordinaryObservation) ids() []string {
	ids := make([]string, len(o.Recipients))
	for i, r := range o.Recipients {
		ids[i] = r.ID
	}
	return ids
}

func (e *Engine) ordinaryRoster(ctx context.Context, ch mattermost.Channel) ([]string, string, error) {
	if ch.Type != "O" {
		ids, err := e.MM.Audience(ctx, ch.ID)
		return ids, "", err
	}
	source, ok := e.MM.(interface {
		TeamAudience(context.Context, string) ([]string, string, error)
	})
	if !ok {
		return nil, "", unavailable()
	}
	return source.TeamAudience(ctx, ch.TeamID)
}

// Observe every reader through the authenticated transport. Public channel
// subscriptions alone omit readers who can open the channel through its team.
func (e *Engine) observeOrdinary(ctx context.Context, w config.Workflow, key conversation.Key) (ordinaryObservation, error) {
	var empty ordinaryObservation
	start := time.Now()
	if !w.OrdinaryUse() || !config.ValidID(e.Bot.ID) || w.Access.BotID != e.Bot.ID ||
		e.SourceID != key.Source || key.Source != w.Mattermost.Source(e.Bot.ID) || !config.ValidID(key.Channel) || key.Workflow != w.ID {
		return empty, unavailable()
	}
	ch, err := e.MM.Channel(ctx, key.Channel)
	if err != nil || ch.ID != key.Channel || ch.Shared == nil || *ch.Shared || ch.DeleteAt != 0 || !protectedChannelType(w, ch.Type) {
		return empty, unavailable()
	}
	ids, revision, err := e.ordinaryRoster(ctx, ch)
	if err != nil || len(ids) < 2 || len(ids) > 1000 || !slices.IsSorted(ids) || !slices.Contains(ids, e.Bot.ID) || ch.Type == "D" && len(ids) != 2 {
		return empty, unavailable()
	}
	recipients := make([]ordinaryRecipient, 0, len(ids))
	for i, id := range ids {
		if !config.ValidID(id) || i > 0 && id == ids[i-1] {
			return empty, unavailable()
		}
		u, err := e.MM.User(ctx, id)
		if err != nil || u.ID != id || u.Username == "" || u.DeleteAt != 0 || id == e.Bot.ID && !u.IsBot {
			return empty, unavailable()
		}
		kind := "human"
		if u.IsBot {
			kind = "bot"
		}
		recipients = append(recipients, ordinaryRecipient{ID: id, Kind: kind})
	}
	after, rev, err := e.ordinaryRoster(ctx, ch)
	if err != nil || !slices.Equal(ids, after) || rev != revision {
		return empty, unavailable()
	}
	latest, err := e.MM.Channel(ctx, key.Channel)
	if err != nil || latest.ID != ch.ID || latest.Type != ch.Type || latest.TeamID != ch.TeamID || latest.DeleteAt != 0 || latest.Shared == nil || *latest.Shared || time.Since(start) >= 30*time.Second {
		return empty, unavailable()
	}
	return ordinaryObservation{Complete: true, Destination: ch.ID, ObservedAt: start.Unix(), Recipients: recipients, Source: key.Source}, nil
}

// An output can answer several mentions. Recover every original author from
// the exact source posts, so revoking one author cannot release a batch reply.
func replyAuthors(w config.Workflow, env conversation.Envelope, posts []mattermost.Post) ([]string, error) {
	actor := env.AccessContext.Subject.ID
	if !w.OrdinaryUse() {
		return []string{actor}, nil
	}
	seen := map[string]bool{}
	var authors []string
	for _, p := range posts {
		if !slices.Contains(env.TriggerIDs, p.ID) {
			continue
		}
		if seen[p.ID] || p.ChannelID != env.Channel || p.Root() != env.Root || p.DeleteAt != 0 || !config.ValidID(p.UserID) ||
			p.Props["from_webhook"] != nil || !slices.Contains(env.Versions, conversation.PostVersion(p)) || p.ID == env.Anchor && p.UserID != actor {
			return nil, errWithholdOutput
		}
		seen[p.ID] = true
		if !slices.Contains(authors, p.UserID) {
			authors = append(authors, p.UserID)
		}
	}
	if len(seen) != len(env.TriggerIDs) || !seen[env.Anchor] {
		return nil, errWithholdOutput
	}
	return authors, nil
}

func (e *Engine) workflowGate(ctx context.Context, w config.Workflow, env conversation.Envelope, audience, authors []string) error {
	if !w.OrdinaryUse() {
		return localGate(ctx, w, env, audience, authors)
	}
	if os.Getenv(directAdmissionMode) != "data" || env.AccessProof == nil || !slices.Equal(audience, env.AccessProof.AudienceSubjects) || len(authors) == 0 {
		return unavailable()
	}
	c, ok := readChannelCredentials()
	if !ok {
		return unavailable()
	}
	o, err := e.observeOrdinary(ctx, w, env.Key())
	if err != nil || !slices.Equal(audience, o.ids()) {
		return unavailable()
	}
	for _, id := range authors {
		if !slices.Contains(o.Recipients, ordinaryRecipient{ID: id, Kind: "human"}) {
			return unavailable()
		}
		o.Actor = id
		if err := ordinaryAdmission(ctx, o, c, channelHTTP); err != nil {
			return err
		}
	}
	return nil
}

func ordinaryAdmission(ctx context.Context, o ordinaryObservation, c channelCredentials, client *http.Client) error {
	fresh := func() bool { age := time.Now().Unix() - o.ObservedAt; return age >= 0 && age < 30 }
	if !fresh() {
		return unavailable()
	}
	b, err := json.Marshal(o)
	if err != nil {
		return unavailable()
	}
	sum := sha256.Sum256(b)
	id := uuid.NewString()
	raw, ok := callAdmission(ctx, client, c, id, "audience.agent_admit", o, 1<<20, "")
	if !ok || !fresh() {
		return unavailable()
	}
	f, ok := exactFields(raw, "ok", "request_id", "result")
	var requestID, purpose, digest string
	if !ok || string(f["ok"]) != "true" || json.Unmarshal(f["request_id"], &requestID) != nil || requestID != id {
		return unavailable()
	}
	r, ok := exactFields(f["result"], "purpose", "allowed", "observation_sha256")
	if !ok || json.Unmarshal(r["purpose"], &purpose) != nil || purpose != "agent_use" || json.Unmarshal(r["observation_sha256"], &digest) != nil || digest != hex.EncodeToString(sum[:]) {
		return unavailable()
	}
	switch string(r["allowed"]) {
	case "true":
		return nil
	case "false":
		return &audienceDenial{code: "channel_policy_denied"}
	default:
		return unavailable()
	}
}
