package service

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

func terminalHandoffHook(r *conversation.Run, token string) {
	end := time.Now().Add(-6 * time.Minute)
	failCalculationFixture(r)
	r.FinishedAt = &end
	r.Hooks[0].StartedAt = new(end.Add(-time.Minute))
	r.Hooks[0].FinishedAt = &end
	r.Hooks[0].DeadlineAt = new(end.Add(time.Minute))
	r.Hooks[0].Output = reportSourceMarker(token, r.ID, "retry-before-upload", "report-retry-v1") +
		"Mattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n"
}

func TestReportJournalHandoffAfterRecoveredRun(t *testing.T) {
	for _, fault := range []string{"none", "schema-outage", "sources-outage", "no-opening", "unsigned", "extra-line", "partial", "attempted", "recorded-result", "changed-readers", "edited-original", "no-ack", "source-revoked"} {
		t.Run(fault, func(t *testing.T) {
			f, checkpoint, proof := historicalPublicationFixture(t)
			methodThread(t, f)
			if len(f.api.attempts) != 5 {
				t.Fatal("missing historical recovery")
			}
			previous := f.api.attempts[4].Envelope
			s, r, token := f.stage(t)
			if fault != "no-opening" {
				code, data := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil)
				if code != http.StatusOK || !bytes.Equal(checkpoint, data) {
					t.Fatal("cannot open recovered run", code)
				}
			}
			if fault == "recorded-result" {
				f.e.MM = completedUploadWithLostResponse{f.mm}
				next := methodCheckpoint(t, f, previous.Report.Number, 24)
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.CheckpointName, next); code == http.StatusOK || f.mm.uploads != 3 {
					t.Fatal("missing recorded uncertain upload")
				}
			}
			last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
			terminalHandoffHook(last, token)
			h := &last.Hooks[0]
			switch fault {
			case "schema-outage":
				h.Output = "report-calculation-v1 schema_data_service_unavailable\n" + h.Output
			case "sources-outage":
				h.Output = "report-calculation-v1 sources_data_service_unavailable\n" + h.Output
			case "unsigned":
				terminalHandoffHook(last, "different-generation")
			case "extra-line":
				h.Output += "unexpected\n"
			case "partial":
				h.Completeness = "unavailable"
			case "attempted":
				h.Output += reportSourceMarker(token, r.ID, "attempted-submission", "report-submission-v1")
			case "changed-readers":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			case "edited-original":
				for i := range f.mm.posts {
					if f.mm.posts[i].ID == proof.Original {
						f.mm.posts[i].EditAt = f.mm.posts[i].CreateAt + 1
					}
				}
			case "no-ack":
				f.api.failure = errors.New("indeterminate core submission")
			}
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			switch fault {
			case "none", "schema-outage", "sources-outage", "source-revoked", "no-ack":
				if len(f.api.attempts) != 6 {
					t.Fatal("verified safe retirement failed to start its successor", len(f.api.attempts))
				}
			default:
				if len(f.api.attempts) != 5 {
					t.Fatal("unverified or possibly published generation started a successor")
				}
				return
			}
			next := f.api.attempts[5].Envelope
			if next.Report.Number != previous.Report.Number || next.Report.Attempt != 3 || next.Report.RetryRun != r.ID ||
				next.Predecessor != r.ID || *next.Report.Prior != *previous.Report.Prior || next.Report.Prior.Position != 23 ||
				next.Report.Storage != previous.Report.Storage || *next.Report.Method != *previous.Report.Method ||
				next.Key() != previous.Key() || next.AccessContext.Subject != previous.AccessContext.Subject ||
				next.AccessContext.InvocationID == previous.AccessContext.InvocationID || !slices.Equal(next.AccessProof.AudienceSubjects, previous.AccessProof.AudienceSubjects) {
				t.Fatal("successor changed inputs or reused the previous authority")
			}
			f.e.pending = nil
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if len(f.api.attempts) != 6 {
				t.Fatal("restarted receiver submitted the handoff again")
			}
			if fault == "no-ack" {
				return
			}
			s2, r2, token2 := f.stage(t)
			if s.ID == s2.ID || r.ID == r2.ID || token == token2 {
				t.Fatal("new run reused the previous session or capability")
			}
			if fault == "source-revoked" {
				f.api.decision = func(string) error { return &orpheus.Error{Status: http.StatusForbidden} }
			}
			f.api.decisions = nil
			code, restored := f.call(t, s2.ID, r2.ID, token2, "checkpoint", "", nil)
			if fault == "source-revoked" {
				if code != http.StatusForbidden || bytes.Equal(checkpoint, restored) || f.mm.uploads != 2 {
					t.Fatal("revoked source disclosed checkpoint data", code)
				}
			} else if code != http.StatusOK || !bytes.Equal(checkpoint, restored) || !slices.Equal(f.api.decisions, []string{"restore", "restore"}) {
				t.Fatal("successor did not restore original bytes with fresh source grants", code)
			}
			last = &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
			terminalHandoffHook(last, token2)
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if len(f.api.attempts) != 6 {
				t.Fatal("attempt four escaped the journal limit")
			}
		})
	}
}
