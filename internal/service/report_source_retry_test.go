package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

const completeSourceValidation = "report-calculation-v1 source_validation\nMattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n"

func reportSourceFailure(t *testing.T) (reportTest, []byte) {
	t.Helper()
	f, approval := prepareMethodTest(t)
	publishMethodResult(t, f, reportjob.CheckpointName, methodCheckpoint(t, f, 1, 11))
	addMethodPost(f, approval)
	methodThread(t, f)
	checkpoint := methodCheckpoint(t, f, 2, 23)
	publishMethodResult(t, f, reportjob.CheckpointName, checkpoint)
	methodThread(t, f)
	f.stage(t)
	run := &f.api.snapshot.Sessions[2].Runs[0]
	failCalculationFixture(run)
	run.Hooks[0].Output = completeSourceValidation
	return f, checkpoint
}

func TestReportSourceRetryUsesBothCommandsAndOneFreshGrant(t *testing.T) {
	f, checkpoint := reportSourceFailure(t)
	old := f.api.attempts[2].Envelope
	firstRun := f.api.snapshot.Sessions[2].Runs[0].ID
	for range 2 {
		methodThread(t, f)
	}
	if len(f.api.attempts) != 4 {
		t.Fatalf("expected a single new admission, got %d total", len(f.api.attempts))
	}
	env := f.api.attempts[3].Envelope
	if !env.Report.Diagnostic || env.Report.Attempt != 1 || env.Report.RetryRun != firstRun || env.Report.Number != old.Report.Number ||
		env.Report.Prior == nil || *env.Report.Prior != *old.Report.Prior || env.Report.Prior.Position != 23 || env.Report.Method == nil ||
		*env.Report.Method != *old.Report.Method || env.Predecessor != firstRun || env.Anchor != old.Anchor || env.Key() != old.Key() ||
		env.AccessContext.Subject != old.AccessContext.Subject || !slices.Equal(old.AccessProof.AudienceSubjects, env.AccessProof.AudienceSubjects) ||
		env.AccessContext.InvocationID == old.AccessContext.InvocationID || env.AccessContext.Assignment == nil {
		t.Fatal("diagnostic admission changed the evidence or reused old authority")
	}
	if _, err := reportAssignment(env, f.card, f.e.SourceID); err != nil {
		t.Fatal("invalid fresh assignment", err)
	}
	s, run, token := f.stage(t)
	f.api.decisions = nil
	code, restored := f.call(t, s.ID, run.ID, token, "checkpoint", "", nil)
	if code != http.StatusOK || !bytes.Equal(restored, checkpoint) || !slices.Equal(f.api.decisions, []string{"restore", "restore"}) {
		t.Fatal("old checkpoint not restored under current rights", code, f.api.decisions)
	}
	target := &f.api.snapshot.Sessions[3].Runs[0]
	failCalculationFixture(target)
	// Even an authenticated temporary pre-upload failure cannot extend this diagnostic.
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("retry-before-upload:" + run.ID))
	target.Hooks[0].Output = fmt.Sprintf("report-retry-v1 %x", mac.Sum(nil))
	for range 2 {
		methodThread(t, f)
	}
	if len(f.api.attempts) != 4 {
		t.Fatal("second diagnostic admission appeared without a new receipt")
	}
}

func TestReportSourceRetryRefusesUncertainOrUnboundFailure(t *testing.T) {
	for _, problem := range []string{"http-denial", "other-reason", "old-generic-failure", "extra-output", "incomplete", "cancelled", "uncertain-post",
		"missing-checkpoint", "wrong-owner-checkpoint", "edited-original", "edited-upgrade", "changed-audience", "missing-assignment", "replaced-signature", "missing-approval"} {
		t.Run(problem, func(t *testing.T) {
			f, _ := reportSourceFailure(t)
			s := &f.api.snapshot.Sessions[2]
			run := &s.Runs[0]
			env := f.api.attempts[2].Envelope
			switch problem {
			case "http-denial":
				run.Hooks[0].Output = "report-http-v1 checkpoint admission 403\nMattermost file hook failed: stage=report_checkpoint; HTTPError: HTTP 403\n"
			case "other-reason":
				run.Hooks[0].Output = strings.Replace(completeSourceValidation, "source_validation", "source_not_granted", 1)
			case "old-generic-failure":
				run.Hooks[0].Output = historicalCalculationOutput
			case "extra-output":
				run.Hooks[0].Output += "unrelated\n"
			case "incomplete":
				run.Hooks[0].Completeness = "truncated"
			case "cancelled":
				run.CancelRequested = true
			case "uncertain-post":
				sid, rid, token := f.stage(t)
				run.Status = "running"
				f.mm.lostReply = true
				if status, _ := f.call(t, sid.ID, rid.ID, token, "result", reportjob.WorkbookName, methodWorkbook(t)); status == 200 || f.mm.delayed == nil {
					t.Fatal("expected an ambiguous native upload", status)
				}
				failCalculationFixture(run)
				mac := hmac.New(sha256.New, []byte(token))
				_, _ = mac.Write([]byte("attempted-submission:" + run.ID))
				run.Hooks[0].Output = fmt.Sprintf("report-submission-v1 %x\n%s", mac.Sum(nil), completeSourceValidation)
			case "missing-checkpoint", "wrong-owner-checkpoint":
				idx := slices.IndexFunc(f.mm.posts, func(p mattermost.Post) bool { return p.ID == env.Report.Prior.Post })
				if idx < 0 {
					t.Fatal("missing fixture checkpoint")
				}
				if problem == "missing-checkpoint" {
					f.mm.posts[idx].DeleteAt = f.mm.posts[idx].CreateAt + 1
				} else {
					f.mm.posts[idx].UserID = f.card.Actor
				}
			case "edited-original", "edited-upgrade":
				id := env.Report.Method.Post
				if problem == "edited-original" {
					id = env.Report.Method.Original
				}
				idx := slices.IndexFunc(f.mm.posts, func(p mattermost.Post) bool { return p.ID == id })
				f.mm.posts[idx].EditAt = f.mm.posts[idx].CreateAt + 2
			case "changed-audience":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			case "missing-assignment", "replaced-signature":
				m := &s.Messages[0]
				record, err := conversation.Decode(m.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if problem == "missing-assignment" {
					record.AccessContext.Assignment = nil
				} else {
					as, err := reportAssignment(record, f.card, f.e.SourceID)
					if err != nil {
						t.Fatal(err)
					}
					as.Confirmation.Receipt = strings.Repeat("0", 64)
					raw, err := json.Marshal(as)
					if err != nil {
						t.Fatal(err)
					}
					*record.AccessContext.Assignment = raw
				}
				m.Metadata, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
			case "missing-approval":
				loadMethodApproval = func(reportjob.Card) (reportjob.MethodApproval, error) {
					return reportjob.MethodApproval{}, os.ErrNotExist
				}
			}
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if len(f.api.attempts) != 3 {
				t.Fatalf("unexpected new admission with %s", problem)
			}
		})
	}
}

func TestReportSourceRetryObeysCurrentReadDenialAndNativeReceipt(t *testing.T) {
	for _, result := range []string{"source-revoked", "confirmed-final", "confirmed-checkpoint"} {
		t.Run(result, func(t *testing.T) {
			f, old := reportSourceFailure(t)
			if result == "source-revoked" {
				methodThread(t, f)
				s, r, token := f.stage(t)
				f.api.decision = func(string) error { return &orpheus.Error{Status: 403, Code: "source_read_unconfirmed"} }
				status, received := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil)
				if status != http.StatusForbidden || bytes.Equal(received, old) || f.mm.uploads != 2 {
					t.Fatal("revoked access disclosed a checkpoint or created an output", status)
				}
				return
			}
			target := &f.api.snapshot.Sessions[2].Runs[0]
			target.Status = "running"
			if result == "confirmed-final" {
				publishMethodResult(t, f, reportjob.WorkbookName, methodWorkbook(t))
			} else {
				publishMethodResult(t, f, reportjob.CheckpointName, methodCheckpoint(t, f, 3, 30))
			}
			failCalculationFixture(target)
			target.Hooks[0].Output = completeSourceValidation
			methodThread(t, f)
			if result == "confirmed-final" {
				if len(f.api.attempts) != 3 {
					t.Fatal("retried a published final")
				}
			} else {
				if len(f.api.attempts) != 4 {
					t.Fatal("did not continue from the acknowledged checkpoint")
				}
				next := f.api.attempts[3].Envelope.Report
				if next.Number != 4 || next.Diagnostic || next.Attempt != 0 || next.Prior.Position != 30 {
					t.Fatal("retried a stage with a genuine newer checkpoint")
				}
			}
		})
	}
}
