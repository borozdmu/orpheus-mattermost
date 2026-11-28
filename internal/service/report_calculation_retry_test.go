package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

const historicalCalculationOutput = "Mattermost file hook failed: stage=unknown; validation failed: report calculation did not finish this stage\n"

func failCalculationFixture(r *conversation.Run) {
	end := time.Now().Add(-3 * time.Minute)
	r.Status = "failed"
	r.FinishedAt = &end
	r.Hooks = []conversation.Hook{{
		Name: "before_run", Status: "failed", Completeness: "complete", Output: historicalCalculationOutput,
		StartedAt: new(end.Add(-time.Minute)), FinishedAt: &end, DeadlineAt: new(end.Add(time.Minute)),
	}}
}

func TestReportHistoricalCalculationEvidence(t *testing.T) {
	for _, fault := range []string{"none", "no-final-line-feed", "first-stage", "no-checkpoint", "attempt", "diagnostic", "running", "cancelled",
		"unrelated-output", "partial", "extra-line", "after-post", "wrong-hook", "two-hooks", "no-start", "before-start", "expired", "no-finish"} {
		t.Run(fault, func(t *testing.T) {
			env := conversation.Envelope{Report: &reportjob.Stage{Number: 2, Prior: &reportjob.Output{}}}
			var r conversation.Run
			failCalculationFixture(&r)
			h := &r.Hooks[0]
			switch fault {
			case "no-final-line-feed":
				h.Output = strings.TrimSuffix(h.Output, "\n")
			case "first-stage":
				env.Report.Number = 1
			case "no-checkpoint":
				env.Report.Prior = nil
			case "attempt":
				env.Report.Attempt = 1
			case "diagnostic":
				env.Report.Diagnostic = true
			case "running":
				r.Status = "running"
			case "cancelled":
				r.CancelRequested = true
			case "unrelated-output":
				h.Output = "validation failed"
			case "partial":
				h.Completeness = "truncated"
			case "extra-line":
				h.Output = "unrecognized line\n" + historicalCalculationOutput
			case "after-post":
				h.Output = historicalCalculationOutput + "\nreport-submission-v1 abc"
			case "wrong-hook":
				h.Name = "after_run"
			case "two-hooks":
				r.Hooks = append(r.Hooks, *h)
			case "no-start":
				h.StartedAt = nil
			case "before-start":
				h.FinishedAt = new(h.StartedAt.Add(-time.Second))
			case "expired":
				h.FinishedAt = h.DeadlineAt
			case "no-finish":
				h.FinishedAt = nil
			}
			if got := legacyCalculationFailure(env, r); got != (fault == "none") {
				t.Fatalf("historical evidence accepted = %t", got)
			}
		})
	}
}

// Publish a checkpoint through the real handler so that an accepted second
// stage points to an actual native receipt from the first one.
func calculationStageTwo(t *testing.T) (reportTest, []byte) {
	t.Helper()
	f := reportExample(t)
	s, r, token := f.stage(t)
	progress, err := json.Marshal(map[string]any{"schema": "orpheus.report-progress.v1", "job_id": f.card.Job, "stage": 1, "position": 8})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := packed(t, map[string]string{"job-progress.json": string(progress), "cache.json": "[]"})
	if status, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.CheckpointName, checkpoint); status != http.StatusOK {
		t.Fatal("publish stage 1 checkpoint", status)
	}
	f.api.snapshot.Sessions[0].Runs[0].Status = "completed"
	if err = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil || len(f.api.attempts) != 2 {
		t.Fatal("stage 2 admission", err)
	}
	f.stage(t)
	failCalculationFixture(&f.api.snapshot.Sessions[1].Runs[0])
	return f, checkpoint
}

func TestReportCalculationDiagnosticGetsOneFreshAdmission(t *testing.T) {
	for _, outcome := range []string{"historical-error", "signed-transient-error", "complete"} {
		t.Run(outcome, func(t *testing.T) {
			f, checkpoint := calculationStageTwo(t)
			old := f.api.attempts[1].Envelope
			failed := f.api.snapshot.Sessions[1].Runs[0]
			for range 2 {
				if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.api.attempts) != 3 {
				t.Fatalf("admissions = %d, want one new admission", len(f.api.attempts))
			}
			fresh := f.api.attempts[2].Envelope
			if !fresh.Report.Diagnostic || fresh.Report.Attempt != 1 || fresh.Report.Number != 2 || fresh.Report.RetryRun != failed.ID ||
				fresh.Predecessor != failed.ID || fresh.Report.Prior == nil || *fresh.Report.Prior != *old.Report.Prior ||
				fresh.Anchor != old.Anchor || fresh.Key() != old.Key() || fresh.AccessContext.Subject != old.AccessContext.Subject ||
				!slices.Equal(fresh.AccessProof.AudienceSubjects, old.AccessProof.AudienceSubjects) ||
				fresh.AccessContext.InvocationID == old.AccessContext.InvocationID {
				t.Fatal("diagnostic admission lost provenance or reused the old invocation")
			}
			if _, err := reportAssignment(fresh, f.card, f.e.SourceID); err != nil {
				t.Fatal("diagnostic assignment", err)
			}
			s, r, token := f.stage(t)
			f.api.decisions = nil
			status, bytesRestored := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil)
			if status != http.StatusOK || !bytes.Equal(bytesRestored, checkpoint) || !slices.Equal(f.api.decisions, []string{"restore", "restore"}) {
				t.Fatal("fresh source checks did not restore the exact checkpoint", status, f.api.decisions)
			}
			target := &f.api.snapshot.Sessions[2].Runs[0]
			if outcome == "complete" {
				wb := packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, wb); code != http.StatusOK {
					t.Fatal("workbook", code)
				}
				target.Status = "completed"
			} else {
				failCalculationFixture(target)
				if outcome == "signed-transient-error" {
					mac := hmac.New(sha256.New, []byte(token))
					_, _ = mac.Write([]byte("retry-before-upload:" + r.ID))
					target.Hooks[0].Output = fmt.Sprintf("report-retry-v1 %x", mac.Sum(nil))
				}
			}
			for range 2 {
				if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.api.attempts) != 3 {
				t.Fatal("diagnostic run was repeated", len(f.api.attempts))
			}
		})
	}
}

func TestReportCalculationDiagnosticRejectsUnboundEvidence(t *testing.T) {
	for _, fault := range []string{"deleted-receipt", "foreign-receipt", "replaced-file", "edited-command", "different-audience", "missing-assignment", "wrong-signature", "wrong-recipient", "late-post"} {
		t.Run(fault, func(t *testing.T) {
			f, _ := calculationStageTwo(t)
			env := f.api.attempts[1].Envelope
			prev := env.Report.Prior
			idx := slices.IndexFunc(f.mm.posts, func(p mattermost.Post) bool { return p.ID == prev.Post })
			if idx < 0 {
				t.Fatal("missing fixture receipt")
			}
			switch fault {
			case "deleted-receipt":
				f.mm.posts[idx].DeleteAt = time.Now().UnixMilli()
			case "foreign-receipt":
				f.mm.posts[idx].UserID = f.card.Actor
			case "replaced-file":
				info := f.mm.infos[prev.File]
				info.PostID = env.Anchor
				f.mm.infos[prev.File] = info
			case "edited-command":
				f.mm.posts[0].Message += " edited"
			case "different-audience":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			case "late-post":
				s, r, token := f.stage(t)
				f.api.snapshot.Sessions[1].Runs[0].Status = "running"
				wb := packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, wb); code != http.StatusOK {
					t.Fatal("native result", code)
				}
				failCalculationFixture(&f.api.snapshot.Sessions[1].Runs[0])
			default:
				m := &f.api.snapshot.Sessions[1].Messages[0]
				env, err := conversation.Decode(m.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "missing-assignment" {
					env.AccessContext.Assignment = nil
				} else {
					as, err := agentcontract.Decode(*env.AccessContext.Assignment)
					if err != nil {
						t.Fatal(err)
					}
					if fault == "wrong-signature" {
						as.Confirmation.Receipt = strings.Repeat("0", 64)
					} else {
						as.Delivery.Thread = strings.Repeat("e", 26)
					}
					raw, err := json.Marshal(as)
					if err != nil {
						t.Fatal(err)
					}
					*env.AccessContext.Assignment = raw
				}
				m.Metadata, err = json.Marshal(env)
				if err != nil {
					t.Fatal(err)
				}
			}
			err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if len(f.api.attempts) != 2 {
				t.Fatal("unexpected admission", err)
			}
		})
	}
}

func TestReportCalculationDiagnosticAdvancesWithAcknowledgedCheckpoint(t *testing.T) {
	f, _ := calculationStageTwo(t)
	if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil || len(f.api.attempts) != 3 {
		t.Fatal("diagnostic admission", err)
	}
	s, r, token := f.stage(t)
	progress, err := json.Marshal(map[string]any{"schema": "orpheus.report-progress.v1", "job_id": f.card.Job, "stage": 2, "position": 12})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := packed(t, map[string]string{"job-progress.json": string(progress), "cache.json": "[]"})
	if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.CheckpointName, checkpoint); code != http.StatusOK {
		t.Fatal("publish diagnostic checkpoint", code)
	}
	f.api.snapshot.Sessions[2].Runs[0].Status = "completed"
	for range 2 {
		if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.api.attempts) != 4 {
		t.Fatal("expected one new regular stage", len(f.api.attempts))
	}
	env := f.api.attempts[3].Envelope
	st := env.Report
	if st.Number != 3 || st.Diagnostic || st.Attempt != 0 || st.RetryRun != "" || env.Predecessor != r.ID ||
		st.Prior == nil || st.Prior.Run != r.ID || st.Prior.Session != s.ID || st.Prior.Position != 12 {
		t.Fatal("next stage has incorrect provenance")
	}
	finalSession, finalRun, finalToken := f.stage(t)
	code, restored := f.call(t, finalSession.ID, finalRun.ID, finalToken, "checkpoint", "", nil)
	if code != http.StatusOK || !bytes.Equal(restored, checkpoint) {
		t.Fatalf("stage three could not restore the diagnostic checkpoint: HTTP %d", code)
	}
}
