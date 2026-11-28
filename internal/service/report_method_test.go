package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

const syntheticMethod = "source-only synthetic replacement"

type methodTestMM struct{ *reportMM }

func (s methodTestMM) Files(ctx context.Context, post string) ([]mattermost.FileInfo, error) {
	p, err := s.Post(ctx, post)
	if err != nil {
		return nil, err
	}
	var infos []mattermost.FileInfo
	for _, id := range p.FileIDs {
		info, ok := s.infos[id]
		if !ok {
			return s.reportMM.Files(ctx, post)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func prepareMethodTest(t *testing.T) (reportTest, reportjob.MethodApproval) {
	t.Helper()
	f := reportExample(t)
	a := reportjob.MethodApproval{Schema: "orpheus.mattermost.method-approval.v1", Job: f.card.Job, Card: f.card.Digest(),
		From: f.card.Method, To: attachments.Hash([]byte(syntheticMethod)), FileName: "mark-mql-adaptive.pyz"}
	saved := loadMethodApproval
	loadMethodApproval = func(card reportjob.Card) (reportjob.MethodApproval, error) { return a, nil }
	t.Cleanup(func() { loadMethodApproval = saved })
	f.e.MM = methodTestMM{f.mm}
	return f, a
}

func addMethodPost(f reportTest, a reportjob.MethodApproval) mattermost.Post {
	const postID = "uuuuuuuuuuuuuuuuuuuuuuuuuu"
	const fileID = "gggggggggggggggggggggggggg"
	post := mattermost.Post{ID: postID, UserID: f.card.Actor, RootID: f.key.Root, ChannelID: f.key.Channel,
		CreateAt: f.mm.posts[0].CreateAt + 10, FileIDs: []string{fileID}, Message: a.Command(f.e.Bot.Username)}
	f.mm.posts = append(f.mm.posts, post)
	f.mm.infos[fileID] = mattermost.FileInfo{ID: fileID, PostID: post.ID, UserID: post.UserID, Size: int64(len(syntheticMethod)), Name: a.FileName, MIME: "application/zip"}
	f.mm.data[fileID] = []byte(syntheticMethod)
	return post
}

func methodCheckpoint(t *testing.T, f reportTest, stage, position int) []byte {
	t.Helper()
	progress, err := json.Marshal(map[string]any{"schema": "orpheus.report-progress.v1", "job_id": f.card.Job, "stage": stage, "position": position})
	if err != nil {
		t.Fatal(err)
	}
	return packed(t, map[string]string{"job-progress.json": string(progress), "cache.json": "[]"})
}

func methodWorkbook(t *testing.T) []byte {
	t.Helper()
	return packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
}

func publishMethodResult(t *testing.T, f reportTest, name string, b []byte) {
	t.Helper()
	s, r, token := f.stage(t)
	if code, _ := f.call(t, s.ID, r.ID, token, "result", name, b); code != http.StatusOK {
		t.Fatal("native publication", code)
	}
	f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1].Runs[0].Status = "completed"
}

func methodThread(t *testing.T, f reportTest) {
	t.Helper()
	if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
		t.Fatal(err)
	}
}

func TestReportMethodChangeRunsWithBothNativeCommands(t *testing.T) {
	f, a := prepareMethodTest(t)
	first := methodCheckpoint(t, f, 1, 11)
	publishMethodResult(t, f, reportjob.CheckpointName, first)
	post := addMethodPost(f, a)
	methodThread(t, f)
	second := methodCheckpoint(t, f, 2, 13)
	for i, prior := range [][]byte{first, second} {
		if len(f.api.attempts) != 2+i {
			t.Fatal("unexpected number of admissions", len(f.api.attempts))
		}
		env := f.api.attempts[1+i].Envelope
		u := env.Report.Method
		if u == nil || !u.Approved(a, f.card) || u.Post != post.ID || u.File != post.FileIDs[0] || u.Original != f.mm.posts[0].ID ||
			env.Anchor != post.ID || env.Report.Number != i+2 || env.Report.Prior.SHA != attachments.Hash(prior) ||
			env.AccessContext.InvocationID == f.api.attempts[i].Envelope.AccessContext.InvocationID {
			t.Fatal("replacement lost original identity, method, or checkpoint provenance")
		}
		transition, ok := env.Report.Manifest()["method_transition"].(map[string]string)
		if ok != (i == 0) || ok && (transition["from_sha256"] != a.From || transition["to_sha256"] != a.To) {
			t.Fatal("incorrect transition on stage", i+2)
		}
		s, r, token := f.stage(t)
		if code, data := f.call(t, s.ID, r.ID, token, "method", "", nil); code != http.StatusOK || string(data) != syntheticMethod {
			t.Fatal("wrong executable", code)
		}
		if code, data := f.call(t, s.ID, r.ID, token, "checkpoint", "", nil); code != http.StatusOK || !bytes.Equal(data, prior) {
			t.Fatal("wrong checkpoint", code)
		}
		if i == 0 {
			publishMethodResult(t, f, reportjob.CheckpointName, second)
		} else {
			workbook := methodWorkbook(t)
			for range 2 {
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, workbook); code != http.StatusOK {
					t.Fatal("final workbook", code)
				}
			}
			f.api.snapshot.Sessions[2].Runs[0].Status = "completed"
		}
		methodThread(t, f)
	}
	methodThread(t, f)
	if len(f.api.attempts) != 3 || f.mm.uploads != 3 || f.mm.calls != 3 {
		t.Fatalf("expected two checkpoint receipts and a single final delivery, got %d runs, %d uploads, %d posts", len(f.api.attempts), f.mm.uploads, f.mm.calls)
	}
}

func TestReportMethodChangeAfterDiagnosticFailure(t *testing.T) {
	f, a := prepareMethodTest(t)
	checkpoint := methodCheckpoint(t, f, 1, 11)
	publishMethodResult(t, f, reportjob.CheckpointName, checkpoint)
	methodThread(t, f)
	f.stage(t)
	failCalculationFixture(&f.api.snapshot.Sessions[1].Runs[0])
	methodThread(t, f)
	if len(f.api.attempts) != 3 || !f.api.attempts[2].Envelope.Report.Diagnostic {
		t.Fatal("expected old method diagnostic")
	}
	f.stage(t)
	r := &f.api.snapshot.Sessions[2].Runs[0]
	failCalculationFixture(r)
	r.Hooks[0].Output = "report-calculation-v1 scoped_source_changed\nMattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n"
	addMethodPost(f, a)
	methodThread(t, f)
	methodThread(t, f)
	if len(f.api.attempts) != 4 {
		t.Fatal("expected one replacement admission", len(f.api.attempts))
	}
	fresh := f.api.attempts[3].Envelope
	if fresh.Report.Method == nil || fresh.Report.Diagnostic || fresh.Report.Number != 2 || fresh.Report.Prior.Position != 11 || fresh.Report.Prior.SHA != attachments.Hash(checkpoint) ||
		fresh.Predecessor != r.ID || fresh.AccessContext.InvocationID == f.api.attempts[2].Envelope.AccessContext.InvocationID {
		t.Fatal("diagnostic resumed from a foreign or unverified checkpoint")
	}
}

func TestReportMethodRefusesUntrustedTransition(t *testing.T) {
	for _, fault := range []string{"foreign-author", "changed-audience", "different-content", "wrong-name", "foreign-file-owner", "two-commands", "two-files", "edited-original", "edited-approval", "prior-missing", "uncertain-post", "already-delivered"} {
		t.Run(fault, func(t *testing.T) {
			f, a := prepareMethodTest(t)
			first := methodCheckpoint(t, f, 1, 11)
			publishMethodResult(t, f, reportjob.CheckpointName, first)
			methodThread(t, f)
			s, r, token := f.stage(t)
			switch fault {
			case "already-delivered":
				publishMethodResult(t, f, reportjob.WorkbookName, methodWorkbook(t))
			case "uncertain-post":
				f.mm.lostReply = true
				if code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, methodWorkbook(t)); code == 200 || f.mm.delayed == nil {
					t.Fatal("failed to simulate a remote operation with no visible receipt")
				}
				target := &f.api.snapshot.Sessions[1].Runs[0]
				failCalculationFixture(target)
				mac := hmac.New(sha256.New, []byte(token))
				_, _ = mac.Write([]byte("attempted-submission:" + r.ID))
				target.Hooks[0].Output = fmt.Sprintf("report-submission-v1 %x", mac.Sum(nil))
			default:
				failCalculationFixture(&f.api.snapshot.Sessions[1].Runs[0])
			}
			post := addMethodPost(f, a)
			cmd := &f.mm.posts[len(f.mm.posts)-1]
			file := post.FileIDs[0]
			info := f.mm.infos[file]
			switch fault {
			case "foreign-author":
				cmd.UserID = strings.Repeat("n", 26)
			case "changed-audience":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			case "different-content":
				f.mm.data[file] = bytes.Repeat([]byte("!"), len(syntheticMethod))
			case "wrong-name":
				info.Name = "other.pyz"
			case "foreign-file-owner":
				info.UserID = f.e.Bot.ID
			case "two-commands":
				post.ID = strings.Repeat("q", 26)
				f.mm.posts = append(f.mm.posts, post)
			case "two-files":
				cmd.FileIDs = append(cmd.FileIDs, strings.Repeat("z", 26))
			case "edited-original":
				f.mm.posts[0].EditAt = f.mm.posts[0].CreateAt + 1
			case "edited-approval":
				cmd.EditAt = cmd.CreateAt + 1
			case "prior-missing":
				f.mm.posts[1].DeleteAt = f.mm.posts[1].CreateAt + 1
			}
			f.mm.infos[file] = info
			_ = f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			for _, admitted := range f.api.attempts[2:] {
				if admitted.Envelope.Report == nil || admitted.Envelope.Report.Method != nil || admitted.Envelope.Anchor == post.ID {
					t.Fatal("untrusted command reached the new method or normal user pipeline")
				}
			}
			if (fault == "uncertain-post" || fault == "already-delivered") && (len(f.api.attempts) != 2 || f.mm.uploads != 2) {
				t.Fatal("uncertain or confirmed publication was repeated")
			}
		})
	}
}

func TestReportMethodRequiresUnchangedCommandsAtByteRelease(t *testing.T) {
	for _, issue := range []string{"original", "approval", "audience", "file-owner"} {
		t.Run(issue, func(t *testing.T) {
			f, a := prepareMethodTest(t)
			publishMethodResult(t, f, reportjob.CheckpointName, methodCheckpoint(t, f, 1, 11))
			post := addMethodPost(f, a)
			methodThread(t, f)
			s, r, token := f.stage(t)
			switch issue {
			case "original":
				f.mm.posts[0].EditAt = f.mm.posts[0].CreateAt + 5
			case "approval":
				i := slices.IndexFunc(f.mm.posts, func(p mattermost.Post) bool { return p.ID == post.ID })
				f.mm.posts[i].EditAt = post.CreateAt + 5
			case "audience":
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			case "file-owner":
				info := f.mm.infos[post.FileIDs[0]]
				info.UserID = f.e.Bot.ID
				f.mm.infos[info.ID] = info
			}
			for _, op := range []string{"method", "checkpoint", "result"} {
				var b []byte
				var name string
				if op == "result" {
					b, name = methodWorkbook(t), reportjob.WorkbookName
				}
				if code, _ := f.call(t, s.ID, r.ID, token, op, name, b); code != http.StatusForbidden {
					t.Fatal("method no longer bound to native consent", op, code)
				}
			}
			if f.mm.uploads != 1 {
				t.Fatal("unauthorized upload")
			}
		})
	}
}
