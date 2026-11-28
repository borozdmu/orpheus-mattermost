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
var loadMethodApproval = reportjob.LoadMethodApproval

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
	storage, err := reportStorage(card, "")
	if err != nil {
		return err
	}
	p.Envelope.Report = &reportjob.Stage{Job: card.Job, Number: 1, Card: card.Digest(), Storage: storage}
	p.Envelope.AccessContext.InvocationID = p.Envelope.Report.Invocation(key.Invocation(p.Envelope.Anchor))
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

// Verify the recorded assignment at issue time for provenance. Current
// authority is evaluated separately for every admission and file exchange.
func reportAssignment(env conversation.Envelope, card reportjob.Card, sourceID string) (agentcontract.Assignment, error) {
	var empty agentcontract.Assignment
	ctx := env.AccessContext
	if ctx == nil || ctx.Assignment == nil || env.AccessProof == nil {
		return empty, errReportBinding
	}
	a, err := agentcontract.Decode(*ctx.Assignment)
	if err != nil || a.Author.ID != card.Actor || a.Author.ID != ctx.Subject.ID || a.Author.Namespace != ctx.Subject.Namespace || a.Confirmation.Revision != env.Revision ||
		!a.Confirmation.ObservedAt.Equal(ctx.ContextObservedAt) || a.InvocationID != ctx.InvocationID || a.Delivery.Connector != "mattermost" || a.Delivery.Message != env.Anchor ||
		a.Delivery.Chat != card.Channel || a.Delivery.Thread != card.Thread || a.Delivery.Source != sourceID || a.Verify(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), a.Confirmation.ObservedAt) != nil {
		return empty, errReportBinding
	}
	return a, nil
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
	var previousSession *conversation.Session
	var prev conversation.Envelope
	var run conversation.Run
	seen := make(map[[3]int]bool)
	for _, s := range snap.Sessions {
		for _, r := range s.Runs {
			env, x := contract(s, r)
			if x != nil || env.Report == nil || env.Report.Job != card.Job {
				continue
			}
			rank := reportRank(env.Report)
			if !env.Report.Valid(card) || seen[rank] {
				return true, errors.New("conflicting report history")
			}
			seen[rank] = true
			if prev.Report == nil || laterReport(rank, reportRank(prev.Report)) {
				previousSession, prev, run = &s, env, r
			}
		}
	}
	if prev.Report == nil || !run.Terminal() || run.CancelRequested || run.Status == "cancelled" {
		return false, nil
	}
	old, err := protectedContract(previousSession, &run, w, key)
	if err != nil || previousSession.ExternalKey != key.External() || run.SessionID != previousSession.ID || !reportInputBound(w, old, card) {
		return true, errReportBinding
	}
	previousAssignment, err := reportAssignment(old, card, e.SourceID)
	if err != nil {
		return true, err
	}
	receipt, artifact, err := e.reportReceipt(ctx, p, prev, run)
	if err != nil {
		return true, err
	}
	if receipt != nil && prev.Report.Storage != "" {
		req := reportRequest{e: e, card: card, env: prev, run: run, assignment: previousAssignment}
		if err = req.recordPublication(ctx, "receipt", artifact.SHA256); err != nil {
			return true, err
		}
	}
	if receipt != nil && artifact.Name != reportjob.CheckpointName {
		return false, nil
	}
	post, err := e.reportCommand(ctx, card, prev)
	if err != nil {
		return true, err
	}
	replacement, err := e.requestedMethod(card, prev, post, p.Posts)
	if err != nil {
		return true, err
	}
	nextStage := reportjob.Stage{Card: card.Digest(), Job: card.Job, Method: prev.Report.Method}
	switch {
	case receipt != nil:
		nextStage.Number = prev.Report.Number + 1
		nextStage.Prior = receipt
	case replacement != nil:
		// A failed old stage can only resume from an already published earlier file.
		if prev.Report.Prior == nil || !provenBeforeSubmission(prev, run) {
			return true, errors.New("report method change has no safe checkpoint boundary")
		}
		req := reportRequest{e: e, w: w, card: card, env: prev, run: run}
		if _, err := req.checkpointReceipt(ctx, snap, p); err != nil {
			return true, err
		}
		nextStage.Number, nextStage.Prior = prev.Report.Number, prev.Report.Prior
	default:
		submitted := reportHookWitness(prev, run, "attempted-submission", "report-submission-v1")
		if submitted && run.FinishedAt != nil && time.Now().Before(run.FinishedAt.Add(2*time.Minute)) {
			// The remote create may have succeeded before its reply was lost.
			// During the reconciliation window we only refresh the native receipt.
			return false, nil
		}
		diagnostic := legacyCalculationFailure(prev, run) || sourceCalculationFailure(prev, run)
		var recovery string
		if !submitted && prev.Report.Diagnostic && run.Error == "service_credentials_expired" {
			journal, openErr := openPublication(card)
			if openErr != nil {
				return true, openErr
			}
			approval := journal.Approval
			if closeErr := journal.File.Close(); closeErr != nil {
				return true, closeErr
			}
			if historicalRecovery(prev, run, previousAssignment, approval, card, time.Now()) {
				recovery = approval.Digest()
			}
		}
		ordinaryRetry := !prev.Report.Diagnostic && prev.Report.Attempt < 3 && (diagnostic || reportHookWitness(prev, run, "retry-before-upload", "report-retry-v1") || legacyInitialMethodRefusal(prev, run))
		if submitted || prev.Report.Recovery != "" || recovery == "" && !ordinaryRetry {
			// Only an explicit digest/position receipt can start a subsequent stage.
			// A single technical notice remains visible if the stage stopped earlier.
			if err := e.reportStop(ctx, w, prev, run, p); err != nil {
				return true, err
			}
			return false, nil
		}
		nextStage = *prev.Report
		nextStage.Attempt++
		nextStage.Diagnostic = diagnostic
		nextStage.Recovery = recovery
		nextStage.RetryRun = run.ID
		notBefore := run.FinishedAt
		if notBefore == nil || time.Now().Before(notBefore.Add(time.Minute*time.Duration(1<<(nextStage.Attempt-1)))) {
			return false, nil
		}
		if nextStage.Prior != nil {
			req := reportRequest{e: e, w: w, card: card, env: prev, run: run}
			if _, err := req.checkpointReceipt(ctx, snap, p); err != nil {
				return true, err
			}
		}
	}
	if receipt == nil && prev.Report.Storage != "" {
		j, openErr := openPublication(card)
		if openErr != nil {
			return true, openErr
		}
		defer func() { _ = j.File.Close() }()
		if prev.Report.Storage != j.Approval.Registration() {
			return true, errReportBinding
		}
		if err = j.CheckUnsubmitted(ctx, reportjob.PublicationEvent{Stage: prev.Report.Number, Run: run.ID, Invocation: previousAssignment.InvocationID.String(), Digest: previousAssignment.Confirmation.Digest}); err != nil {
			return true, err
		}
	}
	nextStage.Storage, err = reportStorage(card, prev.Report.Storage)
	if err != nil {
		return true, err
	}
	if replacement != nil {
		replacement.First = nextStage.Number
		nextStage.Method = replacement
		index := slices.IndexFunc(p.Posts, func(v mattermost.Post) bool { return v.ID == replacement.Post })
		post = p.Posts[index]
	}
	builder := conversation.Builder{Source: e.MM, Config: e.Config, Workflow: w, Bot: e.Bot, TriggerOnly: true}
	env, msgs, err := builder.BuildMessages(ctx, key, channel, []mattermost.Post{post}, conversation.Snapshot{}, true, time.Now())
	if err != nil {
		return true, err
	}
	if env.Anchor != post.ID || len(msgs) != 1 || len(prev.Request.Files) != 1 || len(env.Request.Files) != 1 || replacement == nil && (env.Anchor != prev.Anchor || env.Request.Files[0] != prev.Request.Files[0]) {
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
	if !reportMethodInputBound(env, card) {
		return true, errReportBinding
	}
	env.Predecessor = run.ID
	env.AccessContext.InvocationID = env.Report.Invocation(key.Invocation(env.Anchor))
	next := pending{Workflow: w, Envelope: env, Messages: msgs, Predecessor: run.ID, Channel: channel, TriggerPosts: []mattermost.Post{post}}
	if err := corporateAssignment(&next); err != nil {
		return true, err
	}
	if replacement != nil {
		// Refuse a mismatched native upload before establishing a new method
		// generation. Both consent posts are rechecked after this bounded read.
		if _, err := e.readBoundFile(ctx, w, next.Envelope, env.Request.Files[0], replacement.To); err != nil {
			return true, err
		}
	}
	if nextStage.Recovery != "" {
		if _, err = e.reportCommand(ctx, card, next.Envelope); err != nil {
			return true, err
		}
		if err = claimRecovery(ctx, card, nextStage, next.Envelope.AccessContext.InvocationID.String()); err != nil {
			return true, err
		}
	}
	e.set(key, next)
	return true, e.submit(ctx, key, next, nil, nil)
}

// The old hook printed this exact complete failure only after a non-zero child
// exit and before accessing the outbox. A submitted result or a retriable exit
// printed another line. A diagnostic replay has its own fresh identity and can
// never grant another retry without a confirmed native checkpoint.
func legacyCalculationFailure(env conversation.Envelope, r conversation.Run) bool {
	if env.Report != nil && env.Report.Method != nil {
		return false
	}
	return completeCalculationFailure(env, r, "Mattermost file hook failed: stage=unknown; validation failed: report calculation did not finish this stage\n")
}

// Only the previous receiver's exact, complete pre-submission failure permits
// this diagnostic. The authorization of the replacement method, both native
// commands and the previous checkpoint are validated during admission.
func sourceCalculationFailure(env conversation.Envelope, r conversation.Run) bool {
	if env.Report == nil || env.Report.Method == nil {
		return false
	}
	return completeCalculationFailure(env, r, "report-calculation-v1 source_validation\nMattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n")
}

func completeCalculationFailure(env conversation.Envelope, r conversation.Run, output string) bool {
	s := env.Report
	if s == nil || s.Number < 2 || s.Prior == nil || s.Attempt != 0 || s.Diagnostic || r.Status != "failed" || r.CancelRequested || r.FinishedAt == nil || len(r.Hooks) != 1 {
		return false
	}
	h := r.Hooks[0]
	return h.Name == "before_run" && h.Status == "failed" && h.Completeness == "complete" &&
		h.StartedAt != nil && h.DeadlineAt != nil && h.FinishedAt != nil && h.FinishedAt.After(*h.StartedAt) && h.FinishedAt.Before(*h.DeadlineAt) &&
		h.Output == output
}

// The previous receiver treated a missing model deadline as an expired hook.
// Its first stage had exactly one report_exchange GET (method) and a separate
// report_submission POST. Recognize only its complete pre-calculation failure,
// once, then run the usual fresh native admission from the original command.
func legacyInitialMethodRefusal(env conversation.Envelope, r conversation.Run) bool {
	s := env.Report
	if s == nil || s.Number != 1 || s.Attempt != 0 || s.Prior != nil || r.Status != "failed" || r.CancelRequested || r.DeadlineAt != nil || len(r.Hooks) != 1 {
		return false
	}
	h := r.Hooks[0]
	return h.Name == "before_run" && h.Status == "failed" && h.Completeness == "complete" &&
		h.StartedAt != nil && h.DeadlineAt != nil && h.FinishedAt != nil && !h.FinishedAt.Before(*h.StartedAt) && h.FinishedAt.Before(*h.DeadlineAt) &&
		strings.TrimSpace(h.Output) == "Mattermost file hook failed: stage=report_exchange; HTTPError: HTTP 403"
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
		card, err := loadReportCard()
		if err != nil {
			return err
		}
		if _, err := e.reportCommand(ctx, card, env); err != nil {
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
