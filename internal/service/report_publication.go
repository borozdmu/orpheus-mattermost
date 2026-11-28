package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

var openPublication = reportjob.OpenPublication

func reportStorage(card reportjob.Card, existing string) (string, error) {
	j, err := openPublication(card)
	if err != nil {
		return "", err
	}
	if err = j.File.Close(); err != nil {
		return "", err
	}
	storage := j.Approval.Registration()
	if existing != "" && existing != storage {
		return "", errReportBinding
	}
	return storage, nil
}

func (x *reportRequest) recordPublication(ctx context.Context, action, digest string) error {
	journal, err := openPublication(x.card)
	if err != nil {
		return err
	}
	defer func() { _ = journal.File.Close() }()
	if x.env.Report.Storage == "" || x.env.Report.Storage != journal.Approval.Registration() {
		return errReportBinding
	}
	if x.env.Report.Recovery != "" && x.env.Report.Recovery != journal.Approval.Digest() {
		return errReportBinding
	}
	return journal.Record(ctx, reportjob.PublicationEvent{Stage: x.env.Report.Number, Run: x.run.ID, Invocation: x.assignment.InvocationID.String(), Action: action, Digest: digest})
}

// Only a separately reviewed historical absence can cover a stage that
// predates this ledger. A missing terminal output is never that evidence.
func historicalRecovery(env conversation.Envelope, run conversation.Run, a agentcontract.Assignment, approval reportjob.RecoveryApproval, card reportjob.Card, now time.Time) bool {
	s := env.Report
	if s == nil || !s.Diagnostic || s.Attempt != 1 || s.Recovery != "" || s.Storage != "" || s.Prior == nil || s.Method == nil ||
		run.Status != "failed" || run.CancelRequested || run.Error != "service_credentials_expired" || run.FinishedAt == nil || len(run.Hooks) != 1 {
		return false
	}
	h := run.Hooks[0]
	if h.Name != "before_run" || h.Status != "failed" || h.Completeness != "unavailable" || h.Output != "" || h.StartedAt == nil || h.FinishedAt == nil || h.DeadlineAt == nil ||
		!h.FinishedAt.After(*h.StartedAt) || !h.FinishedAt.Before(*h.DeadlineAt) {
		return false
	}
	return approval.Valid(card) && !now.Before(approval.Approved) && now.Before(approval.Expires) &&
		approval.Run == run.ID && approval.Session == run.SessionID && approval.Invocation == a.InvocationID.String() &&
		approval.Assignment == a.Confirmation.Digest && approval.Stage == s.Number && approval.Original == s.Method.Original && approval.Upgrade == s.Method.Post &&
		approval.Method == s.Method.To && approval.Checkpoint == s.Prior.SHA && approval.CheckpointPost == s.Prior.Post && approval.Position == s.Prior.Position &&
		!approval.RestoreFirst.Before(*h.StartedAt) && !approval.CRM.After(*h.FinishedAt) && !approval.Revoked.Before(*run.FinishedAt)
}

func claimRecovery(ctx context.Context, card reportjob.Card, stage reportjob.Stage, invocation string) error {
	journal, err := openPublication(card)
	if err != nil {
		return err
	}
	defer func() { _ = journal.File.Close() }()
	a := journal.Approval
	if a.Digest() != stage.Recovery || time.Now().Before(a.Approved) || !time.Now().Before(a.Expires) || stage.RetryRun != a.Run {
		return errors.New("recovery approval changed")
	}
	return journal.Record(ctx, reportjob.PublicationEvent{Stage: stage.Number, Run: a.Run, Invocation: invocation, Action: "claim", Digest: a.Digest()})
}

// The signed program returned 75 and the complete trusted hook stopped before
// inspecting its outbox. Submission or any other output is not this witness.
func recoveredCalculationRetry(env conversation.Envelope, run conversation.Run) bool {
	s := env.Report
	if s == nil || s.Recovery == "" || s.Storage == "" || s.Attempt != 2 || s.Diagnostic || s.Prior == nil ||
		run.FinishedAt == nil || len(run.Hooks) != 1 {
		return false
	}
	return signedPreUploadStop(env, run)
}

func signedPreUploadStop(env conversation.Envelope, run conversation.Run) bool {
	if run.Status != "failed" || run.CancelRequested || run.FinishedAt == nil || len(run.Hooks) != 1 {
		return false
	}
	h := run.Hooks[0]
	if h.Name != "before_run" || h.Status != "failed" || h.Completeness != "complete" ||
		h.StartedAt == nil || h.FinishedAt == nil || h.DeadlineAt == nil || !h.FinishedAt.After(*h.StartedAt) ||
		!h.FinishedAt.Before(*h.DeadlineAt) || h.FinishedAt.After(*run.FinishedAt) {
		return false
	}
	output := h.Output
	for _, prefix := range []string{"report-calculation-v1 schema_data_service_unavailable\n", "report-calculation-v1 sources_data_service_unavailable\n"} {
		if rest, ok := strings.CutPrefix(output, prefix); ok {
			output = rest
			break
		}
	}
	witness, rest, ok := strings.Cut(output, "\n")
	return ok && rest == "Mattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n" &&
		strings.HasPrefix(witness, "report-retry-v1 ") && reportHookWitness(env, run, "retry-before-upload", "report-retry-v1")
}

func secondMethodBoundary(env conversation.Envelope, run conversation.Run) bool {
	s := env.Report
	return s != nil && s.Attempt == 3 && !s.Diagnostic && s.Storage != "" && s.Recovery == "" &&
		s.Method != nil && s.Method.Previous == nil && s.Prior != nil && signedPreUploadStop(env, run)
}

func retireForSecondMethod(ctx context.Context, card reportjob.Card, prev conversation.Envelope, run conversation.Run, assignment agentcontract.Assignment, invocation string) error {
	journal, err := openPublication(card)
	if err != nil {
		return err
	}
	defer func() { _ = journal.File.Close() }()
	a, s := journal.Approval, prev.Report
	if !a.Valid(card) || a.Registration() != s.Storage || a.Stage != s.Number || a.Method != s.Method.To ||
		a.Original != s.Method.Original || a.Upgrade != s.Method.Post || a.CheckpointPost != s.Prior.Post ||
		a.Checkpoint != s.Prior.SHA || a.Position != s.Prior.Position {
		return errReportBinding
	}
	return journal.Record(ctx, reportjob.PublicationEvent{Stage: s.Number, Run: run.ID, Invocation: invocation,
		Action: "method-change", Digest: assignment.Confirmation.Digest})
}

func handoffRecovery(ctx context.Context, card reportjob.Card, prev conversation.Envelope, run conversation.Run, assignment agentcontract.Assignment, invocation string) error {
	journal, err := openPublication(card)
	if err != nil {
		return err
	}
	defer func() { _ = journal.File.Close() }()
	if prev.Report.Storage != journal.Approval.Registration() || prev.Report.Recovery != journal.Approval.Digest() {
		return errReportBinding
	}
	// The positive old opening and absence of an external result must still
	// hold under the very lock that removes the former writer's authority.
	return journal.Record(ctx, reportjob.PublicationEvent{Stage: prev.Report.Number, Run: run.ID, Invocation: invocation,
		Action: "handoff", Digest: assignment.Confirmation.Digest})
}
