package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/agentcontract"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

var loadReportCard = reportjob.Load

// attachFirstReport is effective only for a genuine, configured post. Historical
// chat text, operator input or a posted receipt alone cannot start a job.
func (e *Engine) attachFirstReport(ctx context.Context, p *pending, snap conversation.Snapshot) error {
	if !slices.ContainsFunc(p.TriggerPosts, func(post mattermost.Post) bool {
		return strings.HasPrefix(strings.TrimSpace(post.Message), "@"+e.Bot.Username+" /report ")
	}) {
		return nil
	}
	card, err := loadReportCard()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	key := p.Envelope.Key()
	if card.Workflow != key.Workflow || card.Channel != key.Channel || card.Thread != key.Root {
		return nil
	}
	postIndex := slices.IndexFunc(p.TriggerPosts, func(post mattermost.Post) bool {
		return post.ID == p.Envelope.Anchor && post.UserID == card.Actor && reportjob.MatchesCommand(post.Message, e.Bot.Username, card)
	})
	if postIndex < 0 {
		return nil
	}
	if p.Run != "" || len(p.TriggerPosts) != 1 || len(p.Envelope.TriggerIDs) != 1 {
		return errors.New("report command requires a dedicated genuine input")
	}
	isolated, err := e.cleanPending(ctx, *p)
	if err != nil {
		return err
	}
	*p = isolated
	if len(p.Messages) != 1 || !reportInputBound(p.Workflow, p.Envelope, card) {
		return errors.New("report command requires an isolated protected trigger and its method")
	}
	for _, s := range snap.Sessions {
		for _, r := range s.Runs {
			old, readErr := contract(s, r)
			if readErr == nil && old.Report != nil && old.Report.Job == card.Job {
				return errors.New("report card already accepted")
			}
		}
	}
	p.Session = ""
	p.Envelope.Request.PreviousIndex = ""
	p.Envelope.Request.DeliveredBatches = nil
	p.Envelope.Report = &reportjob.Stage{Job: card.Job, Number: 1, Card: card.Digest()}
	return nil
}

func reportInputBound(w config.Workflow, env conversation.Envelope, card reportjob.Card) bool {
	if w.AgentID == "" || w.Access == nil || w.Access.LocalPolicy() || !w.Access.VerifyAccountCards || !slices.Contains(w.Services, "kord") ||
		env.AccessContext == nil || env.AccessProof == nil || env.AccessContext.Subject.ID != card.Actor || env.Channel != card.Channel || env.Root != card.Thread ||
		env.Workflow != card.Workflow || len(env.Request.Files) != 1 || len(env.TriggerIDs) != 1 || env.TriggerIDs[0] != env.Anchor ||
		env.Request.Validate() != nil || env.Request.ChannelID != env.Channel || env.Request.AnchorID != env.Anchor || env.Request.RootID != env.Root || env.Request.SourceID != env.Source {
		return false
	}
	f := env.Request.Files[0]
	return f.PostID == env.Anchor && f.ChannelID == env.Channel && f.Origin == "thread" && f.Status == "" && f.Size > 0 && f.Size <= 1<<20
}

func (e *Engine) continueReport(ctx context.Context, w config.Workflow, key conversation.Key, channel mattermost.Channel, snap conversation.Snapshot, p *delivery.Publisher) (bool, error) {
	history := false
	for _, s := range snap.Sessions {
		for _, r := range s.Runs {
			if env, err := contract(s, r); err == nil && env.Report != nil {
				history = true
				break
			}
		}
		if history {
			break
		}
	}
	if !history {
		return false, nil
	}
	card, err := loadReportCard()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if card.Workflow != key.Workflow || card.Channel != key.Channel || card.Thread != key.Root {
		return false, nil
	}
	var prev conversation.Envelope
	var run conversation.Run
	for _, s := range snap.Sessions {
		for _, r := range s.Runs {
			env, x := contract(s, r)
			if x != nil || env.Report == nil || env.Report.Job != card.Job {
				continue
			}
			if !env.Report.Valid(card) || prev.Report != nil && env.Report.Number == prev.Report.Number && env.Report.Attempt == prev.Report.Attempt {
				return true, errors.New("conflicting report history")
			}
			if prev.Report == nil || prev.Report.Number < env.Report.Number || prev.Report.Number == env.Report.Number && prev.Report.Attempt < env.Report.Attempt {
				prev, run = env, r
			}
		}
	}
	if prev.Report == nil || !run.Terminal() || run.CancelRequested || run.Status == "cancelled" {
		return false, nil
	}
	receipt, artifact, err := e.reportReceipt(ctx, p, prev, run)
	if err != nil {
		return true, err
	}
	nextStage := reportjob.Stage{Card: card.Digest(), Job: card.Job}
	if receipt != nil {
		if artifact.Name != reportjob.CheckpointName {
			return false, nil
		}
		nextStage.Number = prev.Report.Number + 1
		nextStage.Prior = receipt
	} else {
		if reportHookWitness(prev, run, "attempted-submission", "report-submission-v1") && run.FinishedAt != nil && time.Now().Before(run.FinishedAt.Add(2*time.Minute)) {
			// The remote create may have succeeded before its reply was lost.
			// During the reconciliation window we only refresh the native receipt.
			return false, nil
		}
		if !reportHookWitness(prev, run, "retry-before-upload", "report-retry-v1") || prev.Report.Attempt >= 3 {
			// Only an explicit digest/position receipt can start a subsequent stage.
			// A single technical notice remains visible if the stage stopped earlier.
			if err := e.reportStop(ctx, w, prev, run, p); err != nil {
				return true, err
			}
			return false, nil
		}
		nextStage = *prev.Report
		nextStage.Attempt++
		nextStage.RetryRun = run.ID
		notBefore := run.FinishedAt
		if notBefore == nil || time.Now().Before(notBefore.Add(time.Minute*time.Duration(1<<(nextStage.Attempt-1)))) {
			return false, nil
		}
	}
	pos := slices.IndexFunc(p.Posts, func(post mattermost.Post) bool { return post.ID == prev.Anchor })
	if pos < 0 {
		return true, errors.New("original report post no longer present")
	}
	post := p.Posts[pos]
	if post.UserID != card.Actor || post.DeleteAt != 0 || !slices.Contains(prev.Versions, conversation.PostVersion(post)) || !reportjob.MatchesCommand(post.Message, e.Bot.Username, card) {
		return true, errors.New("original report command changed")
	}
	builder := conversation.Builder{Source: e.MM, Config: e.Config, Workflow: w, Bot: e.Bot, TriggerOnly: true}
	env, msgs, err := builder.BuildMessages(ctx, key, channel, []mattermost.Post{post}, conversation.Snapshot{}, true, time.Now())
	if err != nil {
		return true, err
	}
	if env.Anchor != prev.Anchor || len(msgs) != 1 || len(prev.Request.Files) != 1 || len(env.Request.Files) != 1 || env.Request.Files[0] != prev.Request.Files[0] {
		return true, errors.New("report input changed")
	}
	if e.Reserve != nil && !e.Reserve(key, w) {
		return true, nil
	}
	env.AccessContext, env.AccessProof, err = e.admission(ctx, w, key, env, []mattermost.Post{post})
	if err != nil {
		return true, err
	}
	if !reportInputBound(w, env, card) || !slices.Equal(env.AccessProof.AudienceSubjects, prev.AccessProof.AudienceSubjects) || env.AccessProof.ProjectionDigest != prev.AccessProof.ProjectionDigest {
		return true, errors.New("report audience changed")
	}
	env.Report = &nextStage
	env.Predecessor = run.ID
	env.AccessContext.InvocationID = env.Report.Invocation(key.Invocation(env.Anchor))
	next := pending{Workflow: w, Envelope: env, Messages: msgs, Predecessor: run.ID, Channel: channel, TriggerPosts: []mattermost.Post{post}}
	if err := corporateAssignment(&next); err != nil {
		return true, err
	}
	e.set(key, next)
	return true, e.submit(ctx, key, next, nil, nil)
}

func reportHookWitness(env conversation.Envelope, r conversation.Run, purpose, label string) bool {
	if r.Status != "failed" || r.CancelRequested || env.AccessContext == nil || env.AccessContext.Assignment == nil {
		return false
	}
	a, err := agentcontract.Decode(*env.AccessContext.Assignment)
	if err != nil {
		return false
	}
	cap := env.Report.Token(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), a.InvocationID.String(), a.Confirmation.Digest, env.AccessContext.ContextObservedAt)
	if cap == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cap))
	_, _ = mac.Write([]byte(purpose + ":" + r.ID))
	line := label + " " + hex.EncodeToString(mac.Sum(nil))
	return slices.ContainsFunc(r.Hooks, func(h conversation.Hook) bool {
		return h.Name == "before_run" && h.Status == "failed" && h.Completeness == "complete" && slices.Contains(strings.Split(h.Output, "\n"), line)
	})
}

func (e *Engine) reportStop(ctx context.Context, w config.Workflow, env conversation.Envelope, r conversation.Run, p *delivery.Publisher) error {
	if env.AccessContext == nil || env.AccessContext.Assignment == nil {
		return errors.New("missing report origin")
	}
	a, err := agentcontract.Decode(*env.AccessContext.Assignment)
	if err != nil {
		return err
	}
	parts := delivery.Parts(env.Key(), r.SessionID, r.ID, "report-stop:"+r.ID, fmt.Sprintf("Этап %d остановился. Доставка нового файла не подтверждена. Последняя подтверждённая позиция сохранена.", env.Report.Number), env.Render, nil)
	p.BeforeCreate = func(ctx context.Context) error {
		post, err := e.MM.Post(ctx, env.Anchor)
		if err != nil || post.UserID != a.Author.ID || post.Root() != env.Root || post.ChannelID != env.Channel || post.DeleteAt != 0 || !slices.Contains(env.Versions, conversation.PostVersion(post)) {
			return errors.New("report origin changed")
		}
		return e.confirmCorporate(ctx, a)
	}
	defer func() { p.BeforeCreate = nil }()
	for _, part := range parts {
		if err := p.PublishOnce(ctx, part); err != nil {
			return err
		}
	}
	return nil
}
