package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

type reportRequest struct {
	e          *Engine
	w          config.Workflow
	card       reportjob.Card
	env        conversation.Envelope
	run        conversation.Run
	assignment agentcontract.Assignment
}

const reportReasonHeader = "X-Orpheus-Report-Reason"

var errReportStage = errors.New("report stage inactive")
var errReportWindow = errors.New("report authorization window closed")
var errReportOrigin = errors.New("report origin changed")
var errReportBinding = errors.New("report admission mismatch")

func (m *Manager) reportHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(reportReasonHeader, "request")
	w.Header().Set("Cache-Control", "no-store")
	card, err := loadReportCard()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err != nil || r.PathValue("job") != card.Job || token == r.Header.Get("Authorization") ||
		!reportjob.ValidToken(token, os.Getenv("ORPHEUS_AUDIENCE_SECRET"), card.Job, time.Now()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sid, rid := r.Header.Get("X-Orpheus-Session"), r.Header.Get("X-Orpheus-Run")
	if !reportjob.GUID(sid) || !reportjob.GUID(rid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for _, connection := range m.connections {
		e := connection.runtime.Engine
		route, err := e.corporateRoute(card.Channel, "P")
		if err != nil || route.ID != card.Workflow || route.Access == nil || e.SourceID != route.Mattermost.Source(route.Access.BotID) {
			continue
		}
		if !e.reportMu.TryLock() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		defer e.reportMu.Unlock()
		deadline := time.Now().Add(210 * time.Second)
		control := http.NewResponseController(w)
		if err := control.SetWriteDeadline(deadline.Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "stage pending", http.StatusServiceUnavailable)
			return
		}
		if err := control.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "stage pending", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		req, err := e.activeReport(ctx, *route, card, sid, rid, token)
		if err != nil {
			reportFailure(w, err, "admission")
			return
		}
		req.serve(ctx, w, r)
		return
	}
	http.Error(w, "forbidden", http.StatusForbidden)
}

func (e *Engine) activeReport(ctx context.Context, w config.Workflow, card reportjob.Card, sid, rid, token string) (*reportRequest, error) {
	key := conversation.Key{Source: e.SourceID, Workflow: w.ID, Channel: card.Channel, Root: card.Thread}
	if w.Access == nil || w.Draining || w.AgentID == "" {
		return nil, errors.New("report workflow unavailable")
	}
	snap, err := e.API.Snapshot(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, s := range snap.Sessions {
		if s.ID != sid || s.ExternalKey != key.External() {
			continue
		}
		for _, r := range s.Runs {
			if r.ID != rid || r.SessionID != sid {
				continue
			}
			env, err := protectedContract(&s, &r, w, key)
			if err != nil || env.Report == nil || !env.Report.Valid(card) || !reportInputBound(w, env, card) || env.AccessContext.Assignment == nil {
				break
			}
			a, err := reportAssignment(env, card, e.SourceID)
			if err != nil || !hmac.Equal([]byte(token), []byte(env.Report.Token(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), a.InvocationID.String(), a.Confirmation.Digest, env.AccessContext.ContextObservedAt))) {
				break
			}
			reply := reportRequest{e: e, w: w, card: card, env: env, run: r, assignment: a}
			if err = reply.current(ctx); err != nil {
				return nil, err
			}
			return &reply, nil
		}
	}
	return nil, errReportBinding
}

func (x *reportRequest) originCurrent(ctx context.Context) error {
	r, err := x.e.API.Run(ctx, x.run.SessionID, x.run.ID)
	if err != nil {
		return err
	}
	if r.ID != x.run.ID || r.SessionID != x.run.SessionID || r.Terminal() || r.CancelRequested || r.Phase != "before_run" {
		return errReportStage
	}
	now := time.Now()
	if !reportHookActive(r, now) || x.assignment.Verify(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), now) != nil {
		return errReportWindow
	}
	post, err := x.e.MM.Post(ctx, x.env.Anchor)
	if err != nil {
		return err
	}
	if post.ID != x.env.Anchor || post.UserID != x.card.Actor || post.Root() != x.card.Thread || post.ChannelID != x.card.Channel || post.DeleteAt != 0 || !slices.Contains(x.env.Versions, conversation.PostVersion(post)) || !reportjob.MatchesCommand(post.Message, x.e.Bot.Username, x.card) {
		return errReportOrigin
	}
	return nil
}

// Core starts the hook before it starts the model's execution deadline.
func reportHookActive(run conversation.Run, now time.Time) bool {
	if run.DeadlineAt != nil && !run.DeadlineAt.After(now) {
		return false
	}
	found := false
	for _, h := range run.Hooks {
		if h.Name != "before_run" {
			continue
		}
		if found || h.Status != "running" || h.StartedAt == nil || h.StartedAt.After(now) ||
			h.DeadlineAt == nil || !h.DeadlineAt.After(now) || h.FinishedAt != nil {
			return false
		}
		found = true
	}
	return found
}

func (x *reportRequest) current(ctx context.Context) error {
	if err := x.originCurrent(ctx); err != nil {
		return err
	}
	return x.e.confirmCorporate(ctx, x.assignment)
}

func (x *reportRequest) permit(ctx context.Context, sha, purpose string) error {
	if err := x.current(ctx); err != nil {
		return err
	}
	check, ok := x.e.API.(interface {
		CheckSourceRead(context.Context, orpheus.SourceRead, string) error
	})
	if !ok {
		return errors.New("report source authority unavailable")
	}
	err := check.CheckSourceRead(ctx, orpheus.SourceRead{Invocation: x.assignment.InvocationID.String(), Assignment: x.assignment.Confirmation.Digest, Job: x.card.Job, SHA: sha, Source: x.card.Source, Revision: x.card.Revision, Purpose: purpose}, os.Getenv("ORPHEUS_AUDIENCE_SECRET"))
	if err != nil {
		return err
	}
	return x.originCurrent(ctx)
}

func (x *reportRequest) serve(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	switch {
	case r.Method == "GET" && r.PathValue("action") == "method":
		data, err = x.e.readBoundFile(ctx, x.w, x.env, x.env.Request.Files[0], x.card.Method)
	case r.Method == "GET" && r.PathValue("action") == "checkpoint":
		data, err = x.restore(ctx)
	case r.Method == "POST" && r.PathValue("action") == "result":
		limit := min(int64(32<<20), x.w.Files.MaxOutputBytes, x.env.Request.Limits.MaxOutputBytes)
		if r.ContentLength <= 0 || r.ContentLength > limit {
			http.Error(w, "invalid size", http.StatusRequestEntityTooLarge)
			return
		}
		data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		sha := attachments.Hash(data)
		if err != nil || int64(len(data)) != r.ContentLength || !hmac.Equal([]byte(sha), []byte(r.Header.Get("X-Artifact-SHA256"))) {
			http.Error(w, "invalid result", http.StatusBadRequest)
			return
		}
		if err = x.finish(ctx, r.Header.Get("X-Artifact-Name"), data, sha); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"sha256": sha, "status": "delivered"})
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil {
		err = x.originCurrent(ctx)
	}
	if err != nil {
		reportFailure(w, err, r.PathValue("action"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (e *Engine) reportReceipt(ctx context.Context, p *delivery.Publisher, env conversation.Envelope, run conversation.Run) (*reportjob.Output, attachments.Artifact, error) {
	var zero attachments.Artifact
	var out *reportjob.Output
	var artifact attachments.Artifact
	for _, post := range p.Posts {
		if post.UserID != e.Bot.ID || post.DeleteAt != 0 || post.ChannelID != env.Channel || post.Root() != env.Root {
			continue
		}
		var r delivery.Receipt
		if json.Unmarshal(post.Props[delivery.Property], &r) != nil || r.ReportJob != env.Report.Job || r.ReportStage != env.Report.Number {
			continue
		}
		if out != nil || r.Schema != 1 || r.Source != env.Source || r.Workflow != env.Workflow || r.RunID != run.ID || r.SessionID != run.SessionID || r.MessageID != reportMessage(env.Report) || r.Part != 0 || r.Parts != 1 || r.Hash != attachments.Hash([]byte(post.Message)) || len(r.Attachments) != 1 || len(post.FileIDs) != 1 {
			return nil, zero, delivery.ErrConflict
		}
		a := r.Attachments[0]
		if a.FileID != post.FileIDs[0] || !config.ValidID(a.FileID) || a.ArtifactID != attachments.ObjectHash([]string{run.ID, a.Name, a.SHA256}) || a.Size <= 0 || a.Size > env.Request.Limits.MaxOutputBytes ||
			(a.Name != reportjob.WorkbookName && a.Name != reportjob.CheckpointName) || (a.Name == reportjob.CheckpointName && (r.ReportPosition <= 0 || env.Report.Prior != nil && r.ReportPosition <= env.Report.Prior.Position)) {
			return nil, zero, delivery.ErrConflict
		}
		info, err := e.MM.File(ctx, a.FileID)
		if err != nil {
			return nil, zero, err
		}
		if info.PostID != post.ID || info.ID != a.FileID || info.UserID != e.Bot.ID || info.Size != a.Size || info.Name != a.Name {
			return nil, zero, delivery.ErrConflict
		}
		out = &reportjob.Output{Post: post.ID, File: a.FileID, SHA: a.SHA256, Run: run.ID, Session: run.SessionID, Position: r.ReportPosition}
		artifact = a
	}
	return out, artifact, nil
}

func reportMessage(s *reportjob.Stage) string { return fmt.Sprintf("report:%s:%d", s.Job, s.Number) }

func (x *reportRequest) checkpointReceipt(ctx context.Context, snap conversation.Snapshot, p *delivery.Publisher) (attachments.Artifact, error) {
	var zero attachments.Artifact
	prev := x.env.Report.Prior
	if prev == nil || x.env.Report.Number < 2 || prev.Run == x.run.ID || prev.Session == x.run.SessionID {
		return zero, errors.New("missing report checkpoint")
	}
	for _, s := range snap.Sessions {
		if s.ID != prev.Session || s.ExternalKey != x.env.Key().External() {
			continue
		}
		for _, r := range s.Runs {
			if r.ID != prev.Run || r.SessionID != s.ID || !r.Terminal() || r.CancelRequested {
				continue
			}
			source, err := protectedContract(&s, &r, x.w, x.env.Key())
			if err != nil || source.Report == nil || source.Report.Number+1 != x.env.Report.Number || !source.Report.Valid(x.card) || source.Anchor != x.env.Anchor || source.AccessContext.Subject != x.env.AccessContext.Subject || source.AccessProof.ProjectionDigest != x.env.AccessProof.ProjectionDigest || !slices.Equal(source.AccessProof.AudienceSubjects, x.env.AccessProof.AudienceSubjects) {
				continue
			}
			if _, err := reportAssignment(source, x.card, x.e.SourceID); err != nil {
				return zero, err
			}
			out, a, err := x.e.reportReceipt(ctx, p, source, r)
			if err != nil {
				return zero, err
			}
			if out == nil || *out != *prev || a.Name != reportjob.CheckpointName {
				return zero, errors.New("predecessor receipt changed")
			}
			return a, nil
		}
	}
	return zero, errors.New("predecessor unavailable")
}

func (x *reportRequest) restore(ctx context.Context) ([]byte, error) {
	snap, err := x.e.API.Snapshot(ctx, x.env.Key())
	if err != nil {
		return nil, err
	}
	p := &delivery.Publisher{MM: x.e.MM, Key: x.env.Key(), BotID: x.e.Bot.ID}
	if err := p.Refresh(ctx); err != nil {
		return nil, err
	}
	a, err := x.checkpointReceipt(ctx, snap, p)
	if err != nil {
		return nil, err
	}
	if err := x.permit(ctx, a.SHA256, "restore"); err != nil {
		return nil, err
	}
	src, ok := x.e.MM.(interface {
		Download(context.Context, string, int64) ([]byte, error)
	})
	if !ok {
		return nil, errors.New("download unavailable")
	}
	data, err := src.Download(ctx, a.FileID, a.Size)
	if err != nil || int64(len(data)) != a.Size || attachments.Hash(data) != a.SHA256 {
		return nil, errors.New("checkpoint changed")
	}
	if err := x.permit(ctx, a.SHA256, "restore"); err != nil {
		return nil, err
	}
	return data, nil
}

func (x *reportRequest) finish(ctx context.Context, name string, data []byte, sha string) error {
	var position int64
	var err error
	switch name {
	case reportjob.CheckpointName:
		position, err = reportjob.CheckpointPosition(data, *x.env.Report)
		if err != nil {
			return err
		}
	case reportjob.WorkbookName:
		if !reportjob.Workbook(data) {
			return errors.New("invalid report workbook")
		}
	default:
		return errors.New("unexpected report output name")
	}
	p := &delivery.Publisher{MM: x.e.MM, Key: x.env.Key(), BotID: x.e.Bot.ID}
	if err = p.Refresh(ctx); err != nil {
		return err
	}
	old, a, err := x.e.reportReceipt(ctx, p, x.env, x.run)
	if err != nil {
		return err
	}
	if old != nil {
		if old.SHA == sha && a.Name == name && a.Size == int64(len(data)) && old.Position == position {
			return nil
		}
		return delivery.ErrConflict
	}
	if err = x.permit(ctx, sha, "upload"); err != nil {
		return err
	}
	sink, ok := x.e.MM.(interface {
		Upload(context.Context, string, string, io.Reader) (mattermost.FileInfo, error)
	})
	if !ok {
		return errors.New("upload unavailable")
	}
	uploaded, err := sink.Upload(ctx, x.env.Channel, name, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if !config.ValidID(uploaded.ID) || uploaded.PostID != "" || uploaded.UserID != x.e.Bot.ID || uploaded.Name != name || uploaded.Size != int64(len(data)) {
		return errors.New("unexpected report upload")
	}
	a = attachments.Artifact{ArtifactID: attachments.ObjectHash([]string{x.run.ID, name, sha}), FileID: uploaded.ID, Name: name, Path: "snapshot/" + name, SHA256: sha, Size: int64(len(data))}
	text := "Расчёт завершён. Итоговый Excel приложен."
	if name == reportjob.CheckpointName {
		text = fmt.Sprintf("Сохранил промежуточный результат этапа %d. Продолжаю расчёт.", x.env.Report.Number)
	}
	parts := delivery.Parts(x.env.Key(), x.run.SessionID, x.run.ID, reportMessage(x.env.Report), text, x.env.Render, []attachments.Artifact{a})
	if len(parts) != 1 {
		return errors.New("unexpected report delivery split")
	}
	part := parts[0]
	part.Receipt.ReportJob = x.card.Job
	part.Receipt.ReportStage = x.env.Report.Number
	part.Receipt.ReportPosition = position
	p.BeforeCreate = func(ctx context.Context) error { return x.permit(ctx, sha, "publish") }
	if err = p.PublishOnce(ctx, part); err != nil {
		return err
	}
	if err = p.Refresh(ctx); err != nil {
		return err
	}
	old, _, err = x.e.reportReceipt(ctx, p, x.env, x.run)
	if err != nil {
		return err
	}
	if old == nil || old.SHA != sha {
		return errors.New("native report receipt unavailable")
	}
	return nil
}

func reportFailure(w http.ResponseWriter, err error, action string) {
	reason := "request"
	switch {
	case errors.Is(err, errReportStage):
		reason = "stage"
	case errors.Is(err, errReportWindow):
		reason = "window"
	case errors.Is(err, errReportOrigin):
		reason = "origin"
	case errors.Is(err, errReportBinding):
		reason = "binding"
	case action == "admission" || action == "method" || action == "checkpoint" || action == "result":
		reason = action
	}
	w.Header().Set(reportReasonHeader, reason)
	temporary := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if _, ok := errors.AsType[net.Error](err); ok {
		temporary = true
	}
	if code := mattermost.Status(err); code == 429 || code >= 500 {
		temporary = true
	}
	if core, ok := errors.AsType[*orpheus.Error](err); ok && (core.Status == 429 || core.Status >= 500) {
		temporary = true
	}
	if errors.Is(err, orpheus.ErrSourceReadUnavailable) {
		temporary = true
	}
	if temporary {
		http.Error(w, "stage temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "stage unconfirmed", http.StatusForbidden)
}
