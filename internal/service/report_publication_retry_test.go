package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

type completedUploadWithLostResponse struct{ *reportMM }

func (s completedUploadWithLostResponse) Upload(ctx context.Context, channel, name string, body io.Reader) (mattermost.FileInfo, error) {
	if _, err := s.reportMM.Upload(ctx, channel, name, body); err != nil {
		return mattermost.FileInfo{}, err
	}
	return mattermost.FileInfo{}, context.DeadlineExceeded
}

func historicalPublicationFixture(t *testing.T) (reportTest, []byte, *reportjob.RecoveryApproval) {
	t.Helper()
	f, checkpoint := reportSourceFailure(t)
	methodThread(t, f)
	f.stage(t)
	last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1]
	run := &last.Runs[0]
	failCalculationFixture(run)
	run.Error = "service_credentials_expired"
	hook := &run.Hooks[0]
	hook.Completeness, hook.Output = "unavailable", ""

	// Recreate the native signed contract preceding the journal release.
	old := f.api.attempts[len(f.api.attempts)-1]
	old.Envelope.Report.Storage = ""
	old.Envelope.AccessContext.InvocationID = old.Envelope.Report.Invocation(f.key.Invocation(old.Envelope.Anchor))
	if err := corporateAssignment(&old); err != nil {
		t.Fatal(err)
	}
	last.Messages[0].Metadata = testMetadata(old.Envelope)
	assignment, err := reportAssignment(old.Envelope, f.card, f.e.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	stage := old.Envelope.Report
	approved := time.Now().Add(-time.Minute)
	proof := &reportjob.RecoveryApproval{
		Schema: "orpheus.mattermost.report-recovery.v1", Run: run.ID, Session: last.ID,
		Invocation: assignment.InvocationID.String(), Assignment: assignment.Confirmation.Digest,
		Original: stage.Method.Original, Upgrade: stage.Method.Post, Method: stage.Method.To,
		Checkpoint: stage.Prior.SHA, CheckpointPost: stage.Prior.Post, Stage: stage.Number, Position: stage.Prior.Position,
		Audit: strings.Repeat("f", 64), Receiver: reportjob.AuditedReceiverSHA, FirstID: 100, LastID: 138,
		Events: 39, Allows: 2, RestoreFirst: hook.StartedAt.Add(time.Second), RestoreSecond: hook.StartedAt.Add(2 * time.Second),
		CRM: hook.StartedAt.Add(3 * time.Second), Revoked: run.FinishedAt.Add(time.Second), Approved: approved, Expires: approved.Add(time.Hour),
	}
	opener := openPublication
	journal, err := opener(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.File.Close() }()
	proof.Job, proof.Card, proof.Journal = journal.Approval.Job, journal.Approval.Card, journal.Approval.Journal
	proof.Device, proof.Inode = journal.Approval.Device, journal.Approval.Inode
	if !proof.Valid(f.card) {
		t.Fatal("invalid synthetic evidence")
	}
	openPublication = func(card reportjob.Card) (*reportjob.Publication, error) {
		j, err := opener(card)
		if err == nil {
			j.Approval = *proof
		}
		return j, err
	}
	t.Cleanup(func() { openPublication = opener })
	return f, checkpoint, proof
}

func TestReportJournalRecoveryFromNativePosition(t *testing.T) {
	for _, fault := range []string{"none", "wrong-receipt", "incomplete-audit", "expired", "changed-readers", "revoked"} {
		t.Run(fault, func(t *testing.T) {
			f, checkpoint, proof := historicalPublicationFixture(t)
			switch fault {
			case "wrong-receipt":
				proof.Checkpoint = strings.Repeat("9", 64)
			case "incomplete-audit":
				proof.Events--
			case "expired":
				proof.Expires = proof.Approved.Add(time.Second)
			case "changed-readers":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			}
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if fault != "none" && fault != "revoked" {
				if len(f.api.attempts) != 4 || f.mm.uploads != 2 {
					t.Fatal("invalid recovery created an admission or output", len(f.api.attempts), f.mm.uploads)
				}
				return
			}
			if len(f.api.attempts) != 5 {
				t.Fatal("missing fresh admission", len(f.api.attempts))
			}
			newEnv := f.api.attempts[4].Envelope
			if newEnv.Report.Recovery != proof.Digest() || newEnv.Predecessor != proof.Run ||
				newEnv.Report.Prior.Position != 23 || newEnv.Report.Prior.SHA != proof.Checkpoint || newEnv.Key() != f.key ||
				newEnv.Report.Method.Original != proof.Original || newEnv.Report.Method.Post != proof.Upgrade || newEnv.Report.Method.To != proof.Method ||
				newEnv.AccessContext.InvocationID.String() == proof.Invocation {
				t.Fatal("fresh recovery lost source, destination or identity")
			}
			f.e.pending = nil // a restarted receiver discovers an already accepted native run
			methodThread(t, f)
			if len(f.api.attempts) != 5 {
				t.Fatal("accepted recovery was submitted twice")
			}
			s, run, token := f.stage(t)
			f.api.decisions = nil
			if fault == "revoked" {
				f.api.decision = func(string) error { return &orpheus.Error{Status: http.StatusForbidden} }
			}
			code, data := f.call(t, s.ID, run.ID, token, "checkpoint", "", nil)
			if fault == "revoked" {
				if code != http.StatusForbidden || bytes.Equal(data, checkpoint) || f.mm.uploads != 2 {
					t.Fatal("revoked source disclosed data", code)
				}
			} else if code != http.StatusOK || !bytes.Equal(data, checkpoint) || !slices.Equal(f.api.decisions, []string{"restore", "restore"}) {
				t.Fatal("original checkpoint was not restored under two fresh checks", code, f.api.decisions)
			}
		})
	}
}

func TestReportJournalBlocksRepeatedExternalWritesAfterLostResponse(t *testing.T) {
	for _, operation := range []string{"upload", "create"} {
		t.Run(operation, func(t *testing.T) {
			f := reportExample(t)
			if operation == "upload" {
				f.e.MM = completedUploadWithLostResponse{f.mm}
			} else {
				f.mm.lostReply = true
			}
			s, run, capability := f.stage(t)
			checkpoint := methodCheckpoint(t, f, 1, 23)
			submit := func(data []byte) int {
				code, _ := f.call(t, s.ID, run.ID, capability, "result", reportjob.CheckpointName, data)
				return code
			}
			if code := submit(checkpoint); code == http.StatusOK || f.mm.uploads != 1 {
				t.Fatalf("lost %s response returned HTTP %d after %d uploads", operation, code, f.mm.uploads)
			}
			creates := f.mm.calls
			changed := methodCheckpoint(t, f, 1, 24)
			for _, data := range [][]byte{checkpoint, changed, checkpoint} {
				if submit(data) == http.StatusOK || f.mm.uploads != 1 || f.mm.calls != creates {
					t.Fatalf("repeated %s after its response was lost", operation)
				}
			}
			if operation == "create" {
				if creates != 1 || f.mm.delayed == nil {
					t.Fatal("missing delayed delivery")
				}
				f.mm.posts = append(f.mm.posts, *f.mm.delayed)
				if submit(checkpoint) != http.StatusOK || f.mm.uploads != 1 || f.mm.calls != 1 {
					t.Fatal("the exact late receipt was not reconciled without a second write")
				}
				if bytes.Equal(changed, checkpoint) || submit(changed) == http.StatusOK || f.mm.uploads != 1 || f.mm.calls != 1 {
					t.Fatal("different bytes matched a reconciled delivery")
				}
			}
		})
	}
}
