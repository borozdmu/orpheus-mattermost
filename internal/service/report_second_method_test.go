package service

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

const secondProgram = "independent synthetic checkpoint reader"

func installSecondMethodFixture(t *testing.T, f reportTest, source reportjob.Stage) (reportjob.SecondApproval, mattermost.Post) {
	t.Helper()
	first, err := loadMethodApproval(f.card)
	if err != nil {
		t.Fatal(err)
	}
	second := reportjob.SecondApproval{
		Schema: "orpheus.mattermost.method-approval.v2", Card: f.card.Digest(), Job: f.card.Job,
		Previous: first.Digest(), From: first.To, To: attachments.Hash([]byte(secondProgram)),
		Checkpoint: source.Prior.SHA, Stage: source.Number, Position: source.Prior.Position, FileName: "revised-source.pyz",
	}
	previousLoader := loadSecondApproval
	loadSecondApproval = func(card reportjob.Card, old reportjob.MethodApproval) (reportjob.SecondApproval, error) {
		if !second.Valid(card, old) {
			return reportjob.SecondApproval{}, errReportBinding
		}
		return second, nil
	}
	t.Cleanup(func() { loadSecondApproval = previousLoader })
	previous, err := f.mm.Post(t.Context(), source.Method.Post)
	if err != nil {
		t.Fatal(err)
	}
	file := strings.Repeat("j", 26)
	post := mattermost.Post{ID: strings.Repeat("w", 26), UserID: f.card.Actor, ChannelID: f.card.Channel, RootID: f.key.Root,
		CreateAt: previous.CreateAt + 5, FileIDs: []string{file}, Message: second.Command(f.e.Bot.Username)}
	f.mm.posts = append(f.mm.posts, post)
	f.mm.infos[file] = mattermost.FileInfo{ID: file, PostID: post.ID, UserID: post.UserID, Size: int64(len(secondProgram)), Name: second.FileName, MIME: "application/zip"}
	f.mm.data[file] = []byte(secondProgram)
	return second, post
}

func openPriorAndStop(t *testing.T, f reportTest, checkpoint []byte, attempt int, open bool) {
	t.Helper()
	methodThread(t, f)
	prev := f.api.attempts[len(f.api.attempts)-1].Envelope.Report
	if prev.Attempt != attempt || prev.Prior.Position != 23 || prev.Prior.SHA != attachments.Hash(checkpoint) {
		t.Fatalf("expected authenticated attempt %d at previous native checkpoint", attempt)
	}
	s, r, token := f.stage(t)
	if open {
		code, body := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil)
		if code != http.StatusOK || !bytes.Equal(body, checkpoint) {
			t.Fatal("cannot restore original checkpoint", code)
		}
	}
	last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
	terminalHandoffHook(last, token)
}

const markerSizeStop = "report-calculation-v1 marker_size_rejected\nMattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n"

// Recreate the actual sequence with invented files and identities. The old
// registration authorizes stage ten at position 23. Stage eleven must instead
// prove its own opening and the original stage-ten publication at position 24.
func positionedSecondMethod(t *testing.T, opened bool) (reportTest, []byte, *reportjob.RecoveryApproval) {
	t.Helper()
	f, old, proof := historicalPublicationAt(t, 9)
	openPriorAndStop(t, f, old, 2, true)
	methodThread(t, f)
	previous := f.api.attempts[len(f.api.attempts)-1].Envelope
	if previous.Report.Number != 10 || previous.Report.Attempt != 3 || previous.Report.Prior.Position != 23 {
		t.Fatal("wrong historical stage/attempt")
	}
	s, r, token := f.stage(t)
	if code, b := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(b, old) {
		t.Fatal("cannot restore 23", code)
	}
	data := methodCheckpoint(t, f, 10, 24)
	publishMethodResult(t, f, reportjob.CheckpointName, data)
	methodThread(t, f)
	stage := f.api.attempts[len(f.api.attempts)-1].Envelope.Report
	if stage.Number != 11 || stage.Attempt != 0 || stage.Diagnostic || !stage.Method.Match(*previous.Report.Method) || stage.Prior.Position != 24 || stage.Prior.SHA != attachments.Hash(data) || stage.Prior.Post == proof.CheckpointPost {
		t.Fatal("ordinary stage did not preserve its native predecessor")
	}
	s, r, token = f.stage(t)
	if opened {
		if code, b := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(b, data) {
			t.Fatal("cannot restore 24", code)
		}
	}
	last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
	failCalculationFixture(last)
	last.Hooks[0].Output = markerSizeStop
	return f, data, proof
}

func TestSecondReportMethodFromVerifiedCheckpoint(t *testing.T) {
	f, checkpoint, proof := positionedSecondMethod(t, true)
	before := len(f.api.attempts)
	uploads, posts := f.mm.uploads, f.mm.calls
	previous := f.api.attempts[before-1].Envelope
	prevRun := f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0].ID
	second, post := installSecondMethodFixture(t, f, *previous.Report)
	first, _ := loadMethodApproval(f.card)
	methodThread(t, f)
	f.e.pending = nil
	methodThread(t, f)
	if len(f.api.attempts) != before+1 {
		t.Fatal("expected exactly one admission from the third genuine post", len(f.api.attempts))
	}
	fresh := f.api.attempts[before].Envelope
	rev := fresh.Report.Method
	if !fresh.Report.ApprovedSecond(f.card, first, second) || rev.Previous == nil || !rev.Previous.Match(*previous.Report.Method) || rev.Original != proof.Upgrade || rev.Post != post.ID || fresh.Anchor != post.ID ||
		fresh.Report.Number != previous.Report.Number || fresh.Report.Attempt != 0 || fresh.Report.Diagnostic || fresh.Report.RetryRun != "" || fresh.Predecessor != prevRun ||
		*fresh.Report.Prior != *previous.Report.Prior || fresh.AccessContext.InvocationID == previous.AccessContext.InvocationID || fresh.Key() != previous.Key() || fresh.AccessContext.Subject != previous.AccessContext.Subject || !slices.Equal(fresh.AccessProof.AudienceSubjects, previous.AccessProof.AudienceSubjects) {
		t.Fatal("revision did not preserve the original receipt, person and recipients with a fresh admission")
	}
	transition, ok := fresh.Report.Manifest()["method_transition"].(map[string]string)
	if !ok || transition["from_sha256"] != second.From || transition["to_sha256"] != second.To {
		t.Fatal("first replacement stage lacks correct transition")
	}
	next := methodCheckpoint(t, f, 11, 25)
	for i, expected := range [][]byte{checkpoint, next} {
		s, r, token := f.stage(t)
		if code, b := f.call(t, s.ID, r.ID, token, "method", "", nil); code != http.StatusOK || string(b) != secondProgram {
			t.Fatal("wrong second executable", i, code)
		}
		f.api.decisions = nil
		if code, b := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(b, expected) || !slices.Equal(f.api.decisions, []string{"restore", "restore"}) {
			t.Fatal("restore used wrong bytes or insufficient current permissions", i, code, f.api.decisions)
		}
		if i == 0 {
			publishMethodResult(t, f, reportjob.CheckpointName, next)
			methodThread(t, f)
			if len(f.api.attempts) != before+2 {
				t.Fatal("replacement checkpoint did not start one normal stage", len(f.api.attempts))
			}
			later := f.api.attempts[before+1].Envelope
			if !later.Report.ApprovedSecond(f.card, first, second) || later.Report.Number != 12 || later.Report.Attempt != 0 || later.AccessContext.InvocationID == fresh.AccessContext.InvocationID {
				t.Fatal("new stage lost method provenance")
			}
			if _, ok := later.Report.Manifest()["method_transition"]; ok {
				t.Fatal("transition repeated on ordinary successor")
			}
		} else {
			publishMethodResult(t, f, reportjob.WorkbookName, methodWorkbook(t))
			methodThread(t, f)
			methodThread(t, f)
		}
	}
	if len(f.api.attempts) != before+2 || f.mm.uploads != uploads+2 || f.mm.calls != posts+2 {
		t.Fatal("successor duplicated a published checkpoint or final workbook", len(f.api.attempts), f.mm.uploads, f.mm.calls)
	}
}

func TestSecondReportMethodRejectsUntrustedHistory(t *testing.T) {
	for _, fault := range []string{"no-opening", "changed-audience", "other-author", "edited-initial", "edited-first", "edited-second", "changed-first-card", "wrong-file", "duplicate-command", "no-native-checkpoint", "completed-output", "uncertain-output", "attempted-marker", "unsigned-generic", "transient", "source-denied", "partial", "missing-final-newline", "cancelled", "wrong-stage", "wrong-position", "wrong-checkpoint"} {
		t.Run(fault, func(t *testing.T) {
			f, _, proof := positionedSecondMethod(t, fault != "no-opening")
			current := *f.api.attempts[len(f.api.attempts)-1].Envelope.Report
			// The result route must see an active hook to make a real outbox attempt.
			f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0].Status = "running"
			s, r, token := f.stage(t)
			if fault == "completed-output" {
				publishMethodResult(t, f, reportjob.WorkbookName, methodWorkbook(t))
			}
			if fault == "uncertain-output" {
				f.e.MM = completedUploadWithLostResponse{f.mm}
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, methodWorkbook(t)); code == http.StatusOK {
					t.Fatal("unknown upload expected")
				}
			}
			last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
			failCalculationFixture(last)
			last.Hooks[0].Output = markerSizeStop
			requested := current
			alternate := *current.Prior
			requested.Prior = &alternate
			switch fault {
			case "wrong-stage":
				requested.Number--
			case "wrong-position":
				alternate.Position--
			case "wrong-checkpoint":
				alternate.SHA = proof.Checkpoint
			}
			_, post := installSecondMethodFixture(t, f, requested)
			index := func(id string) int {
				return slices.IndexFunc(f.mm.posts, func(p mattermost.Post) bool { return p.ID == id })
			}
			switch fault {
			case "changed-audience":
				f.mm.audience = append(f.mm.audience, strings.Repeat("y", 26))
			case "other-author":
				f.mm.posts[index(post.ID)].UserID = strings.Repeat("y", 26)
			case "edited-initial", "edited-first", "edited-second":
				id := map[string]string{"edited-initial": proof.Original, "edited-first": proof.Upgrade, "edited-second": post.ID}[fault]
				p := &f.mm.posts[index(id)]
				p.EditAt = p.CreateAt + 1
			case "changed-first-card":
				before := loadMethodApproval
				loadMethodApproval = func(c reportjob.Card) (reportjob.MethodApproval, error) {
					a, e := before(c)
					a.FileName = "different.pyz"
					return a, e
				}
				t.Cleanup(func() { loadMethodApproval = before })
			case "wrong-file":
				f.mm.data[post.FileIDs[0]] = []byte("unreviewed executable")
			case "duplicate-command":
				post.ID = strings.Repeat("y", 26)
				f.mm.posts = append(f.mm.posts, post)
			case "no-native-checkpoint":
				f.mm.posts[index(current.Prior.Post)].DeleteAt = 1
			case "unsigned-generic":
				last.Hooks[0].Output = historicalCalculationOutput
			case "transient":
				terminalHandoffHook(last, token)
			case "source-denied":
				last.Hooks[0].Output = strings.Replace(markerSizeStop, "marker_size_rejected", "source_denied", 1)
			case "partial":
				last.Hooks[0].Completeness = "truncated"
			case "missing-final-newline":
				last.Hooks[0].Output = strings.TrimSuffix(markerSizeStop, "\n")
			case "cancelled":
				last.CancelRequested = true
			case "attempted-marker":
				last.Hooks[0].Output += reportSourceMarker(token, r.ID, "attempted-submission", "report-submission-v1")
			}
			before := len(f.api.attempts)
			uploads := f.mm.uploads
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if len(f.api.attempts) != before || f.mm.uploads != uploads {
				t.Fatal("unconfirmed transition started or uploaded a result", len(f.api.attempts), before)
			}
		})
	}
}

func TestSecondReportMethodKeepsLateNativeCheckpoint(t *testing.T) {
	f, checkpoint, proof := historicalPublicationFixture(t)
	openPriorAndStop(t, f, checkpoint, 2, true)
	methodThread(t, f)
	s, r, capability := f.stage(t)
	if code, data := f.call(t, s.ID, r.ID, capability, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(checkpoint, data) {
		t.Fatal("last attempt did not open the genuine checkpoint", code)
	}
	nextCheckpoint := methodCheckpoint(t, f, proof.Stage, 24)
	f.mm.lostReply = true
	if code, _ := f.call(t, s.ID, r.ID, capability, "result", reportjob.CheckpointName, nextCheckpoint); code == http.StatusOK || f.mm.delayed == nil {
		t.Fatal("checkpoint must have been accepted remotely without a visible receipt")
	}
	last := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0]
	terminalHandoffHook(last, capability)
	previous := f.api.attempts[len(f.api.attempts)-1].Envelope
	_, _ = installSecondMethodFixture(t, f, *previous.Report)
	before := len(f.api.attempts)
	_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
	if len(f.api.attempts) != before {
		t.Fatal("unknown native outcome incorrectly started another method")
	}
	f.mm.lostReply = false
	f.mm.posts = append(f.mm.posts, *f.mm.delayed)
	methodThread(t, f)
	if len(f.api.attempts) != before+1 {
		t.Fatalf("late real checkpoint did not start the ordinary successor: got %d", len(f.api.attempts))
	}
	successor := f.api.attempts[before].Envelope
	if successor.Report.Number != proof.Stage+1 || successor.Report.Attempt != 0 ||
		!successor.Report.Method.Match(*previous.Report.Method) || successor.Report.Method.Previous != nil ||
		successor.Anchor != previous.Anchor || successor.Report.Prior == nil || successor.Report.Prior.Position != 24 ||
		successor.Report.Prior.SHA != attachments.Hash(nextCheckpoint) || successor.AccessContext.InvocationID == previous.AccessContext.InvocationID {
		t.Fatal("late receipt did not keep the original active method and its latest position")
	}
	s2, r2, cap2 := f.stage(t)
	if code, data := f.call(t, s2.ID, r2.ID, cap2, "method", "", nil); code != http.StatusOK || string(data) != syntheticMethod {
		t.Fatal("received wrong method after the late checkpoint", code)
	}
	if code, received := f.call(t, s2.ID, r2.ID, cap2, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(nextCheckpoint, received) {
		t.Fatal("ordinary successor did not restore the later bytes", code)
	}
}

func TestSecondReportMethodDoesNotUseEarlierExhaustedAttempt(t *testing.T) {
	f, checkpoint, _ := historicalPublicationAt(t, 9)
	openPriorAndStop(t, f, checkpoint, 2, true)
	openPriorAndStop(t, f, checkpoint, 3, true)
	before := len(f.api.attempts)
	old := f.api.attempts[before-1].Envelope
	installSecondMethodFixture(t, f, *old.Report)
	_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
	if len(f.api.attempts) != before {
		t.Fatal("old exhausted retry authorized a different method without a stage eleven stop")
	}
}
