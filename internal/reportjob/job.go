// Package reportjob defines a native report stage pinned to one genuine post.
package reportjob

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const BasePath = "/mattermost-report/v1/jobs/"
const CheckpointName = "report-checkpoint.zip"
const WorkbookName = "MQL_2026_RetailCRM.xlsx"

var id = regexp.MustCompile(`^[a-z0-9]{26}$`)
var sha = regexp.MustCompile(`^[a-f0-9]{64}$`)
var source = regexp.MustCompile(`^[a-zA-Z0-9_:.+-]{1,128}$`)

type Card struct {
	Schema   string `json:"schema"`
	Job      string `json:"job_id"`
	Workflow string `json:"workflow"`
	Channel  string `json:"channel_id"`
	Thread   string `json:"root_post_id"`
	Actor    string `json:"actor_id"`
	Method   string `json:"method_sha256"`
	Source   string `json:"source_id"`
	Revision string `json:"source_revision"`
	URL      string `json:"gateway_url"`
}

func Load() (Card, error) {
	var card Card
	file := os.Getenv("MM_REPORT_JOB_CARD")
	if file == "" {
		return card, os.ErrNotExist
	}
	parent, err := os.Lstat(filepath.Dir(file))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 || parent.Sys().(*syscall.Stat_t).Uid != 0 {
		return card, errors.New("report card directory untrusted")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return card, errors.New("report card unavailable")
	}
	raw, err := readCardFile(f)
	if err != nil {
		return card, err
	}
	if !exactJSONFields(raw, "schema", "job_id", "workflow", "channel_id", "root_post_id", "actor_id", "method_sha256", "source_id", "source_revision", "gateway_url") {
		return card, errors.New("invalid report card fields")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&card) != nil || d.Decode(new(any)) != io.EOF || !card.Valid() {
		return Card{}, errors.New("invalid report card")
	}
	return card, nil
}

// readCardFile takes ownership of an open file in a verified operator directory.
func readCardFile(f *os.File) ([]byte, error) {
	defer func() { _ = f.Close() }()
	st, statErr := f.Stat()
	if statErr != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.New("report card untrusted")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		return nil, errors.New("invalid report card size")
	}
	return raw, nil
}

func (c Card) Valid() bool {
	u, e := url.Parse(c.URL)
	return c.Schema == "orpheus.mattermost.report-card.v1" && GUID(c.Job) && id.MatchString(c.Channel) && id.MatchString(c.Thread) && id.MatchString(c.Actor) &&
		source.MatchString(c.Workflow) && sha.MatchString(c.Method) && source.MatchString(c.Source) && source.MatchString(c.Revision) &&
		e == nil && u.Scheme == "https" && u.Host == "beta-kord.skillum.ai" && u.User == nil && u.Path == BasePath && u.RawPath == "" && u.RawQuery == "" && u.Fragment == ""
}

func (c Card) Digest() string {
	data, _ := json.Marshal(c)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func (c Card) Command(username string) string { return "@" + username + " /report " + c.Job }
func GUID(s string) bool                      { u, e := uuid.Parse(s); return e == nil && u != uuid.Nil && u.String() == s }

type Output struct {
	Post     string `json:"post_id"`
	File     string `json:"file_id"`
	SHA      string `json:"sha256"`
	Position int64  `json:"position"`
	Run      string `json:"run_id"`
	Session  string `json:"session_id"`
}

// MethodRevision binds a single reviewed replacement to both real commands.
type MethodRevision struct {
	Approval       string `json:"approval_sha256"`
	Original       string `json:"original_post_id"`
	OriginalHash   string `json:"original_post_sha256"`
	OriginalEdited int64  `json:"original_edit_at"`
	Post           string `json:"approval_post_id"`
	PostHash       string `json:"approval_post_sha256"`
	File           string `json:"method_file_id"`
	From           string `json:"from_sha256"`
	To             string `json:"to_sha256"`
	First          int    `json:"first_stage"`
}

func (u MethodRevision) valid(card Card) bool {
	return u.First >= 2 && id.MatchString(u.Original) && id.MatchString(u.Post) && u.Post != u.Original &&
		id.MatchString(u.File) && sha.MatchString(u.OriginalHash) && u.OriginalEdited >= 0 && sha.MatchString(u.PostHash) &&
		sha.MatchString(u.Approval) && u.From == card.Method && sha.MatchString(u.To) && u.To != u.From
}

func (u MethodRevision) Approved(a MethodApproval, c Card) bool {
	return u.valid(c) && a.Valid(c) && u.Approval == a.Digest() && u.From == a.From && u.To == a.To
}

type Stage struct {
	Card       string          `json:"card_sha256"`
	Job        string          `json:"job_id"`
	Number     int             `json:"stage"`
	Attempt    int             `json:"attempt,omitzero"`
	Diagnostic bool            `json:"diagnostic_replay,omitzero"`
	RetryRun   string          `json:"retry_run,omitzero"`
	Prior      *Output         `json:"previous,omitempty"`
	Method     *MethodRevision `json:"method_revision,omitempty"`
}

func (s Stage) Valid(c Card) bool {
	if s.Card != c.Digest() || s.Job != c.Job || s.Number < 1 || s.Attempt < 0 || s.Attempt > 3 || s.Attempt == 0 && s.RetryRun != "" || s.Attempt > 0 && !GUID(s.RetryRun) {
		return false
	}
	if s.Method != nil && (!s.Method.valid(c) || s.Number < s.Method.First) {
		return false
	}
	if s.Diagnostic && (s.Number < 2 || s.Attempt != 1) {
		return false
	}
	if s.Number == 1 {
		return s.Prior == nil
	}
	p := s.Prior
	return p != nil && id.MatchString(p.Post) && id.MatchString(p.File) && sha.MatchString(p.SHA) && p.Position > 0 && GUID(p.Run) && GUID(p.Session)
}

// Material contains no member, file or business data. A caller must also prove
// that Core is currently running this exact session, run and accepted stage.
func (s Stage) Token(secret, invocation, assignment string, observed time.Time) string {
	if secret == "" || !GUID(invocation) || !sha.MatchString(assignment) || observed.IsZero() {
		return ""
	}
	b, _ := json.Marshal([]any{"orpheus.mattermost.report-stage.v1", s.Job, s.Number, invocation, assignment, observed.Unix()})
	body := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(body))
	return body + "." + hex.EncodeToString(h.Sum(nil))
}

func (s Stage) Invocation(anchor uuid.UUID) uuid.UUID {
	if s.Method != nil {
		domain := "orpheus.mattermost.method-invocation.v1"
		if s.Diagnostic {
			domain = "orpheus.mattermost.method-diagnostic-invocation.v1"
		}
		b, _ := json.Marshal([]any{domain, s.Job, s.Number, s.Method, s.Prior, s.Attempt, s.RetryRun})
		return uuid.NewSHA1(anchor, b)
	}
	if s.Number == 1 && s.Attempt == 0 {
		return anchor
	}
	domain := "orpheus.mattermost.report-invocation.v1"
	if s.Diagnostic {
		domain = "orpheus.mattermost.report-diagnostic-invocation.v1"
	}
	b, _ := json.Marshal([]any{domain, s.Job, s.Number, s.Prior, s.Attempt, s.RetryRun})
	return uuid.NewSHA1(anchor, b)
}

func (s Stage) Manifest() map[string]any {
	previous, digest, position := "", "", int64(0)
	if s.Prior != nil {
		previous, digest, position = s.Prior.Run, s.Prior.SHA, s.Prior.Position
	}
	manifest := map[string]any{"schema": "orpheus.report-job.v1", "job_id": s.Job, "stage": s.Number, "previous_run": previous, "checkpoint_sha256": digest, "position": position}
	if s.Method != nil && s.Number == s.Method.First {
		manifest["method_transition"] = map[string]string{"from_sha256": s.Method.From, "to_sha256": s.Method.To}
	}
	return manifest
}

func ValidToken(token, secret, job string, now time.Time) bool {
	if secret == "" || len(token) > 800 {
		return false
	}
	body, sig, ok := strings.Cut(token, ".")
	proof, err := hex.DecodeString(sig)
	if !ok || err != nil || len(proof) != 32 {
		return false
	}
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(body))
	if !hmac.Equal(proof, h.Sum(nil)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	var fields []json.RawMessage
	if err != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 6 {
		return false
	}
	var tag, id string
	var at int64
	return json.Unmarshal(fields[0], &tag) == nil && tag == "orpheus.mattermost.report-stage.v1" &&
		json.Unmarshal(fields[1], &id) == nil && id == job && json.Unmarshal(fields[5], &at) == nil &&
		at <= now.Unix() && now.Unix()-at < 1200
}
func MatchesCommand(value, username string, card Card) bool {
	return strings.TrimSpace(value) == card.Command(username)
}
