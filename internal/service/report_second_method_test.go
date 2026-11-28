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

func installSecondMethodFixture(t *testing.T, f reportTest, proof *reportjob.RecoveryApproval) (reportjob.SecondApproval, mattermost.Post) {
	t.Helper()
	first, err := loadMethodApproval(f.card)
	if err != nil {
		t.Fatal(err)
	}
	second := reportjob.SecondApproval{
		Schema: "orpheus.mattermost.method-approval.v2", Card: f.card.Digest(), Job: f.card.Job,
		Previous: first.Digest(), From: first.To, To: attachments.Hash([]byte(secondProgram)),
		Checkpoint: proof.Checkpoint, Stage: proof.Stage, Position: proof.Position, FileName: "revised-source.pyz",
	}
	previousLoader := loadSecondApproval
	loadSecondApproval = func(card reportjob.Card, old reportjob.MethodApproval) (reportjob.SecondApproval, error) {
		if !second.Valid(card, old) {
			return reportjob.SecondApproval{}, errReportBinding
		}
		return second, nil
	}
	t.Cleanup(func() { loadSecondApproval = previousLoader })
	previous, err := f.mm.Post(t.Context(), proof.Upgrade)
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

func TestSecondReportMethodFromVerifiedCheckpoint(t *testing.T) {
	f, checkpoint, proof := historicalPublicationFixture(t)
	openPriorAndStop(t, f, checkpoint, 2, true)
	openPriorAndStop(t, f, checkpoint, 3, true)
	previous := f.api.attempts[len(f.api.attempts)-1].Envelope
	prevRun := f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0].ID
	second, post := installSecondMethodFixture(t, f, proof)
	first, _ := loadMethodApproval(f.card)
	methodThread(t, f)
	f.e.pending = nil
	methodThread(t, f)
	if len(f.api.attempts) != 7 {
		t.Fatal("expected exactly one admission from the third genuine post", len(f.api.attempts))
	}
	fresh := f.api.attempts[6].Envelope
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
	next := methodCheckpoint(t, f, proof.Stage, 24)
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
			if len(f.api.attempts) != 8 {
				t.Fatal("replacement checkpoint did not start one normal stage", len(f.api.attempts))
			}
			later := f.api.attempts[7].Envelope
			if !later.Report.ApprovedSecond(f.card, first, second) || later.Report.Number != proof.Stage+1 || later.Report.Attempt != 0 || later.AccessContext.InvocationID == fresh.AccessContext.InvocationID {
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
	if len(f.api.attempts) != 8 || f.mm.uploads != 4 || f.mm.calls != 4 {
		t.Fatal("successor duplicated a published checkpoint or final workbook", len(f.api.attempts), f.mm.uploads, f.mm.calls)
	}
}

func TestSecondReportMethodRejectsUntrustedHistory(t *testing.T) {
	for _, fault := range []string{"no-opening", "changed-audience", "other-author", "edited-initial", "edited-first", "edited-second", "changed-first-card", "wrong-file", "duplicate-command", "no-native-checkpoint", "completed-output", "uncertain-output", "attempted-marker"} {
		t.Run(fault, func(t *testing.T) {
			f, checkpoint, proof := historicalPublicationFixture(t)
			openPriorAndStop(t, f, checkpoint, 2, true)
			methodThread(t, f)
			s, r, token := f.stage(t)
			if fault != "no-opening" {
				if code, body := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(body, checkpoint) {
					t.Fatal("fixture restore", code)
				}
			}
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
			terminalHandoffHook(last, token)
			_, post := installSecondMethodFixture(t, f, proof)
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
				f.mm.posts[index(proof.CheckpointPost)].DeleteAt = 1
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
	_, _ = installSecondMethodFixture(t, f, proof)
	previous := f.api.attempts[len(f.api.attempts)-1].Envelope
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
