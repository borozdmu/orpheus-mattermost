package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

type reportAPI struct {
	*accessAPI
	decisions []string
	decision  func(string) error
}

func (a *reportAPI) Run(_ context.Context, sid, rid string) (conversation.Run, error) {
	for _, s := range a.snapshot.Sessions {
		if s.ID == sid {
			for _, r := range s.Runs {
				if r.ID == rid {
					return r, nil
				}
			}
		}
	}
	return conversation.Run{}, errors.New("unknown run")
}
func (a *reportAPI) CheckSourceRead(_ context.Context, v orpheus.SourceRead, _ string) error {
	a.decisions = append(a.decisions, v.Purpose)
	if a.decision != nil {
		return a.decision(v.Purpose)
	}
	return nil
}

type reportMM struct {
	*acceptedFileSource
	infos     map[string]mattermost.FileInfo
	data      map[string][]byte
	uploads   int
	lostReply bool
	delayed   *mattermost.Post
}

func (s *reportMM) File(_ context.Context, id string) (mattermost.FileInfo, error) {
	i, ok := s.infos[id]
	if !ok {
		return i, &mattermost.HTTPError{Status: 404}
	}
	return i, nil
}
func (s *reportMM) Upload(_ context.Context, ch, name string, r io.Reader) (mattermost.FileInfo, error) {
	if ch != s.channel.ID {
		return mattermost.FileInfo{}, errors.New("wrong target")
	}
	b, e := io.ReadAll(r)
	if e != nil {
		return mattermost.FileInfo{}, e
	}
	s.uploads++
	id := fmt.Sprintf("%026d", s.uploads)
	i := mattermost.FileInfo{ID: id, Name: name, UserID: s.bot, Size: int64(len(b))}
	s.infos[id], s.data[id] = i, b
	return i, nil
}
func (s *reportMM) Create(ctx context.Context, p mattermost.CreatePost) (mattermost.Post, error) {
	post, e := s.fakeSource.Create(ctx, p)
	if e != nil {
		return post, e
	}
	for _, id := range p.FileIDs {
		i := s.infos[id]
		i.PostID = post.ID
		s.infos[id] = i
	}
	if s.lostReply && len(p.FileIDs) > 0 {
		s.posts = s.posts[:len(s.posts)-1]
		s.delayed = &post
		return mattermost.Post{}, context.DeadlineExceeded
	}
	return post, nil
}
func (s *reportMM) Download(_ context.Context, id string, limit int64) ([]byte, error) {
	if b, ok := s.data[id]; ok && int64(len(b)) == limit {
		return slices.Clone(b), nil
	}
	return nil, &mattermost.HTTPError{Status: 404}
}

type reportTest struct {
	e       *Engine
	api     *reportAPI
	mm      *reportMM
	key     conversation.Key
	w       config.Workflow
	card    reportjob.Card
	handler http.Handler
}

func reportExample(t *testing.T) reportTest {
	t.Helper()
	corporateDirectory(t)
	e, base, src, _, key, w := protectedFixture(t)
	w.PrivateChannels = true
	w.AgentID = "mark"
	w.Services = []string{"kord"}
	w.Access.VerifyAccountCards = true
	w.Access.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	w.Access.IdentityProjection.ActorField = "actor_user_id"
	w.Access.IdentityProjection.AudienceField = "reader_user_ids"
	root := corporateStateBinding(w, key.Channel)
	e.Config.Workflows[0] = root
	w, err := root.ForChat(key.Channel)
	if err != nil {
		t.Fatal(err)
	}
	key.Source = w.Mattermost.Source(src.bot)
	e.SourceID = key.Source
	card := reportjob.Card{Schema: "orpheus.mattermost.report-card.v1", Job: uuid.NewString(), Workflow: w.ID, Channel: key.Channel, Thread: key.Root, Actor: src.posts[0].UserID, Method: attachments.Hash([]byte("png")), Source: "my", Revision: "dummy-v1", URL: "https://beta-kord.skillum.ai" + reportjob.BasePath}
	old := loadReportCard
	loadReportCard = func() (reportjob.Card, error) { return card, nil }
	t.Cleanup(func() { loadReportCard = old })
	const program = "ffffffffffffffffffffffffff"
	src.posts[0].FileIDs = []string{program}
	src.posts[0].Message = card.Command(e.Bot.Username)
	mm := &reportMM{acceptedFileSource: &acceptedFileSource{accessSource: src}, infos: map[string]mattermost.FileInfo{}, data: map[string][]byte{program: []byte("png")}}
	api := &reportAPI{accessAPI: base}
	e.MM = mm
	e.API = api
	if err = e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.attempts) != 1 || api.attempts[0].Envelope.Report == nil {
		t.Fatal("missing initial job")
	}
	mgr := Manager{connections: map[config.Endpoint]connection{w.Mattermost: {runtime: &Runtime{Engine: e}}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mattermost-report/v1/jobs/{job}/{action}", mgr.reportHTTP)
	return reportTest{e, api, mm, key, w, card, mux}
}
func (f reportTest) stage(t *testing.T) (conversation.Session, conversation.Run, string) {
	t.Helper()
	s := &f.api.snapshot.Sessions[len(f.api.snapshot.Sessions)-1]
	r := &s.Runs[0]
	s.ExternalKey = f.key.External()
	r.Phase = "before_run"
	r.DeadlineAt = nil
	r.Hooks = []conversation.Hook{{Name: "before_run", Status: "running", StartedAt: new(time.Now().Add(-time.Second)), DeadlineAt: new(time.Now().Add(899 * time.Second))}}
	env, _ := contract(*s, *r)
	a, err := agentcontract.Decode(*env.AccessContext.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	return *s, *r, env.Report.Token(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), a.InvocationID.String(), a.Confirmation.Digest, env.AccessContext.ContextObservedAt)
}
func (f reportTest) call(t *testing.T, sid, rid, token, action, name string, b []byte) (int, []byte) {
	t.Helper()
	method := "GET"
	if b != nil {
		method = "POST"
	}
	r := httptest.NewRequest(method, reportjob.BasePath+f.card.Job+"/"+action, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Orpheus-Session", sid)
	r.Header.Set("X-Orpheus-Run", rid)
	if name != "" {
		r.Header.Set("X-Artifact-Name", name)
		r.Header.Set("X-Artifact-SHA256", attachments.Hash(b))
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func packed(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for n, v := range entries {
		f, e := w.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(f, v); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestReportActiveStagesAndSingleNativeDelivery(t *testing.T) {
	f := reportExample(t)
	s, r, token := f.stage(t)
	get := func(action string, expect int) {
		t.Helper()
		status, _ := f.call(t, s.ID, r.ID, token, action, "", nil)
		if status != expect {
			t.Fatalf("%s %d", action, status)
		}
	}
	get("method", 200)
	get("checkpoint", 403)
	status, _ := f.call(t, s.ID, uuid.NewString(), token, "method", "", nil)
	if status != 403 {
		t.Fatal("foreign run")
	}
	msg, _ := json.Marshal(map[string]any{"schema": "orpheus.report-progress.v1", "job_id": f.card.Job, "stage": 1, "position": 14})
	checkpoint := packed(t, map[string]string{"job-progress.json": string(msg), "cache.json": "[]"})
	for range 2 {
		status, _ = f.call(t, s.ID, r.ID, token, "result", reportjob.CheckpointName, checkpoint)
		if status != 200 {
			t.Fatal("checkpoint", status)
		}
	}
	if f.mm.uploads != 1 || f.mm.calls != 1 || !slices.Equal(f.api.decisions, []string{"upload", "publish"}) {
		t.Fatal("duplicate or unverified checkpoint", f.mm.uploads, f.mm.calls, f.api.decisions)
	}
	f.api.snapshot.Sessions[0].Runs[0].Status = "completed"
	if e := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); e != nil {
		t.Fatal(e)
	}
	if len(f.api.attempts) != 2 || f.api.attempts[1].Envelope.Report.Number != 2 {
		t.Fatal("no confirmed continuation")
	}
	second, newRun, secondCap := f.stage(t)
	status, restored := f.call(t, second.ID, newRun.ID, secondCap, "checkpoint", "", nil)
	if status != 200 || !bytes.Equal(restored, checkpoint) {
		t.Fatal("checkpoint restore", status)
	}
	status, _ = f.call(t, s.ID, r.ID, token, "method", "", nil)
	if status != 403 {
		t.Fatal("terminal stage remains usable")
	}
	workbook := packed(t, map[string]string{"[Content_Types].xml": "<Types/>", "xl/workbook.xml": "<workbook/>", "xl/worksheets/sheet1.xml": "<sheet/>"})
	for range 2 {
		status, _ = f.call(t, second.ID, newRun.ID, secondCap, "result", reportjob.WorkbookName, workbook)
		if status != 200 {
			t.Fatal("final result", status)
		}
	}
	if f.mm.calls != 2 || f.mm.uploads != 2 || !slices.Equal(f.api.decisions, []string{"upload", "publish", "restore", "restore", "upload", "publish"}) {
		t.Fatal("unexpected native deliveries", f.mm.calls, f.mm.uploads, f.api.decisions)
	}
	f.api.snapshot.Sessions[1].Runs[0].Status = "completed"
	if e := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); e != nil {
		t.Fatal(e)
	}
	if len(f.api.attempts) != 2 {
		t.Fatal("workbook resubmitted")
	}
}
func TestReportCheckAtSourceBoundaries(t *testing.T) {
	for _, kind := range []string{"denied", "unavailable", "edited-during-upload-check", "cancelled-during-publish-check", "new-reader"} {
		t.Run(kind, func(t *testing.T) {
			f := reportExample(t)
			s, r, token := f.stage(t)
			f.api.decision = func(p string) error {
				switch {
				case kind == "denied":
					return &orpheus.Error{Status: 403, Code: "source_read_unconfirmed"}
				case kind == "unavailable":
					return orpheus.ErrSourceReadUnavailable
				case kind == "edited-during-upload-check" && p == "upload":
					f.mm.posts[0].Message += "changed"
				case kind == "cancelled-during-publish-check" && p == "publish":
					f.api.snapshot.Sessions[0].Runs[0].CancelRequested = true
				}
				return nil
			}
			if kind == "new-reader" {
				f.mm.audience = append(f.mm.audience, strings.Repeat("n", 26))
			}
			data := packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
			code, _ := f.call(t, s.ID, r.ID, token, "result", reportjob.WorkbookName, data)
			want := 403
			if kind == "unavailable" {
				want = 503
			}
			if code != want || f.mm.calls != 0 || (f.mm.uploads == 1) != (kind == "cancelled-during-publish-check") {
				t.Fatalf("status %d uploads %d posts %d", code, f.mm.uploads, f.mm.calls)
			}
		})
	}
}

func TestReportOutlivesOrdinaryHTTPWriteTimeout(t *testing.T) {
	f := reportExample(t)
	s, run, token := f.stage(t)
	f.api.decision = func(p string) error {
		if p == "upload" {
			time.Sleep(11 * time.Second)
		}
		return nil
	}
	server := httptest.NewUnstartedServer(f.handler)
	server.Config.WriteTimeout = 10 * time.Second
	server.Start()
	defer server.Close()
	data := packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+reportjob.BasePath+f.card.Job+"/result", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Orpheus-Session", s.ID)
	req.Header.Set("X-Orpheus-Run", run.ID)
	req.Header.Set("X-Artifact-Name", reportjob.WorkbookName)
	req.Header.Set("X-Artifact-SHA256", attachments.Hash(data))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || f.mm.calls != 1 {
		t.Fatal(resp.StatusCode, f.mm.calls)
	}
}

func TestReportRestoresOnlyAuthenticatedPredecessor(t *testing.T) {
	for _, fault := range []string{"wrong-creator", "replaced-output", "revoked-after-download"} {
		t.Run(fault, func(t *testing.T) {
			f := reportExample(t)
			s, run, token := f.stage(t)
			msg, _ := json.Marshal(map[string]any{"schema": "orpheus.report-progress.v1", "job_id": f.card.Job, "stage": 1, "position": 8})
			data := packed(t, map[string]string{"job-progress.json": string(msg), "cache.json": "[]"})
			if code, _ := f.call(t, s.ID, run.ID, token, "result", reportjob.CheckpointName, data); code != 200 {
				t.Fatal(code)
			}
			f.api.snapshot.Sessions[0].Runs[0].Status = "completed"
			if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
				t.Fatal(err)
			}
			s, run, token = f.stage(t)
			id := f.api.attempts[1].Envelope.Report.Prior.File
			switch fault {
			case "wrong-creator":
				meta := f.mm.infos[id]
				meta.UserID = f.card.Actor
				f.mm.infos[id] = meta
			case "replaced-output":
				f.mm.data[id][len(data)-1] ^= 1
			case "revoked-after-download":
				calls := 0
				f.api.decision = func(p string) error {
					if p == "restore" {
						calls++
					}
					if calls == 2 {
						return &orpheus.Error{Status: 403, Code: "source_read_unconfirmed"}
					}
					return nil
				}
			}
			if code, b := f.call(t, s.ID, run.ID, token, "checkpoint", "", nil); code != 403 || bytes.Equal(b, data) {
				t.Fatalf("unexpected disclosure: %d", code)
			}
		})
	}
}

func TestReportRetryRequiresFailedSignedBeforeUpload(t *testing.T) {
	for _, kind := range []string{"known-no-upload", "unsigned", "incomplete", "wrong-run", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := reportExample(t)
			_, r, cap := f.stage(t)
			target := &f.api.snapshot.Sessions[0].Runs[0]
			target.Status = "failed"
			target.FinishedAt = new(time.Now().Add(-2 * time.Minute))
			key := cap
			rid := r.ID
			if kind == "unsigned" {
				key = "untrusted"
			}
			if kind == "wrong-run" {
				rid = uuid.NewString()
			}
			mac := hmac.New(sha256.New, []byte(key))
			_, _ = mac.Write([]byte("retry-before-upload:" + rid))
			hook := conversation.Hook{Name: "before_run", Status: "failed", Completeness: "complete", Output: fmt.Sprintf("report-retry-v1 %x", mac.Sum(nil))}
			if kind == "incomplete" {
				hook.Completeness = "truncated"
			}
			if kind == "cancelled" {
				target.CancelRequested = true
			}
			target.Hooks = []conversation.Hook{hook}
			if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
				t.Fatal(err)
			}
			if kind == "known-no-upload" {
				if len(f.api.attempts) != 2 {
					t.Fatal("retry missing")
				}
				next := f.api.attempts[1].Envelope
				if next.Report.Attempt != 1 || next.Report.Number != 1 || next.Report.RetryRun != r.ID || next.Predecessor != r.ID || next.Anchor != f.api.attempts[0].Envelope.Anchor || f.mm.calls != 0 {
					t.Fatal("retry not bound")
				}
				return
			}
			if len(f.api.attempts) != 1 || f.mm.calls > 1 {
				t.Fatal("retried without a reliable before-upload witness")
			}
		})
	}
}

func TestReportReconcilesLateNativeReceiptWithoutCreatingAgain(t *testing.T) {
	f := reportExample(t)
	s, run, token := f.stage(t)
	f.mm.lostReply = true
	data := packed(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x", "xl/worksheets/s.xml": "x"})
	status, _ := f.call(t, s.ID, run.ID, token, "result", reportjob.WorkbookName, data)
	if status == 200 || f.mm.calls != 1 || f.mm.delayed == nil {
		t.Fatal("missing write attempt", status, f.mm.calls)
	}
	r := &f.api.snapshot.Sessions[0].Runs[0]
	r.Status = "failed"
	r.FinishedAt = new(time.Now())
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("attempted-submission:" + r.ID))
	r.Hooks = []conversation.Hook{{Name: "before_run", Status: "failed", Completeness: "complete", Output: fmt.Sprintf("report-submission-v1 %x", mac.Sum(nil))}}
	if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if f.mm.calls != 1 || f.mm.uploads != 1 || len(f.api.attempts) != 1 {
		t.Fatal("uncertain result resubmitted")
	}
	f.mm.posts = append(f.mm.posts, *f.mm.delayed)
	if err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true); err != nil {
		t.Fatal(err)
	}
	if f.mm.calls != 1 || f.mm.uploads != 1 || len(f.api.attempts) != 1 {
		t.Fatal("late published workbook resubmitted")
	}
}

func TestReportHookRealDeadline(t *testing.T) {
	now := time.Now()
	h := conversation.Hook{Name: "before_run", Status: "running", StartedAt: new(now.Add(-time.Second)), DeadlineAt: new(now.Add(899 * time.Second))}
	r := conversation.Run{ID: uuid.NewString(), SessionID: uuid.NewString(), Status: "running", Phase: "before_run", Hooks: []conversation.Hook{h}}
	if !reportHookActive(r, now) {
		t.Fatal("Core before_run has a separate live hook deadline and a null model deadline")
	}
	for _, test := range []string{"missing-hook", "duplicate", "expired-model", "expired-hook", "missing-deadline", "future-start", "finished", "not-running"} {
		t.Run(test, func(t *testing.T) {
			bad := r
			bad.Hooks = slices.Clone(r.Hooks)
			switch test {
			case "missing-hook":
				bad.Hooks = nil
			case "duplicate":
				bad.Hooks = append(bad.Hooks, h)
			case "expired-model":
				bad.DeadlineAt = new(now)
			case "expired-hook":
				bad.Hooks[0].DeadlineAt = new(now)
			case "missing-deadline":
				bad.Hooks[0].DeadlineAt = nil
			case "future-start":
				bad.Hooks[0].StartedAt = new(now.Add(time.Second))
			case "finished":
				bad.Hooks[0].FinishedAt = new(now)
			case "not-running":
				bad.Hooks[0].Status = "pending"
			}
			if reportHookActive(bad, now) {
				t.Fatal("expired/inactive execution accepted")
			}
		})
	}
}

func TestReportMethodReasonIsBounded(t *testing.T) {
	f := reportExample(t)
	s, run, token := f.stage(t)
	q := httptest.NewRequest("GET", reportjob.BasePath+f.card.Job+"/method", nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("X-Orpheus-Run", run.ID)
	q.Header.Set("X-Orpheus-Session", s.ID)
	f.api.snapshot.Sessions[0].Runs[0].Hooks[0].DeadlineAt = nil
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, q)
	if w.Code != 403 || w.Header().Get(reportReasonHeader) != "window" {
		t.Fatal(w.Code, w.Header().Get(reportReasonHeader))
	}
}

func TestReportPriorMethodFailureGetsOneFreshAdmission(t *testing.T) {
	for _, kind := range []string{"initial", "late-failure", "edited", "foreign", "signed-method-denial", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			f := reportExample(t)
			_, first, _ := f.stage(t)
			r := &f.api.snapshot.Sessions[0].Runs[0]
			r.Status = "failed"
			r.FinishedAt = new(time.Now().Add(-2 * time.Minute))
			h := &r.Hooks[0]
			h.StartedAt = new(r.FinishedAt.Add(-time.Second))
			h.DeadlineAt = new(h.StartedAt.Add(900 * time.Second))
			h.FinishedAt = r.FinishedAt
			h.Status = "failed"
			h.Completeness = "complete"
			h.Output = "Mattermost file hook failed: stage=report_exchange; HTTPError: HTTP 403"
			switch kind {
			case "late-failure":
				h.Output = "Mattermost file hook failed: stage=report_submission; HTTPError: HTTP 403"
			case "edited":
				f.mm.posts[0].Message += " edited"
			case "foreign":
				f.mm.audience = append(f.mm.audience, strings.Repeat("f", 26))
			case "signed-method-denial":
				h.Output = "report-http-v1 method window 403\nMattermost file hook failed: stage=report_method; HTTPError: HTTP 403"
			case "incomplete":
				h.Completeness = "truncated"
			}
			err := f.e.Thread(t.Context(), f.w, f.key, f.mm.channel, true)
			if kind != "initial" {
				if len(f.api.attempts) != 1 {
					t.Fatal("false recovery", err)
				}
				return
			}
			if err != nil || len(f.api.attempts) != 2 {
				t.Fatal("no fresh admission", err)
			}
			next := f.api.attempts[1].Envelope
			if next.Report.Attempt != 1 || next.Report.Number != 1 || next.Predecessor != first.ID || next.Anchor != f.api.attempts[0].Envelope.Anchor || next.AccessContext.InvocationID == f.api.attempts[0].Envelope.AccessContext.InvocationID || next.AccessContext.Assignment == nil {
				t.Fatal("reused authorization")
			}
			_, retry, _ := f.stage(t)
			retry.DeadlineAt = nil
			retry.Status = "failed"
			retry.Hooks = []conversation.Hook{*h}
			if legacyInitialMethodRefusal(next, retry) {
				t.Fatal("legacy failure reused")
			}
		})
	}
}
