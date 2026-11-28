package reportjob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const journalHeader = "orpheus.mattermost.publication.v1 "

// This attestation format proves absence of an upload only for a receiver
// whose two mandatory restore checks and separate upload check were audited.
const AuditedReceiverSHA = "d0264b7599aa584394156d5b1d420111a2ee98bb78bdf3117e3e84eeb271a527"

// RecoveryApproval is the operator's statement about one historic run. Its
// file identity pins the publication journal independently of its contents.
type RecoveryApproval struct {
	Schema         string    `json:"schema"`
	Job            string    `json:"job_id"`
	Card           string    `json:"card_sha256"`
	Journal        string    `json:"journal_id"`
	Device         uint64    `json:"device"`
	Inode          uint64    `json:"inode"`
	Run            string    `json:"run_id"`
	Session        string    `json:"session_id"`
	Invocation     string    `json:"invocation_id"`
	Assignment     string    `json:"assignment_sha256"`
	Original       string    `json:"original_post"`
	Upgrade        string    `json:"upgrade_post"`
	Method         string    `json:"method_sha256"`
	Checkpoint     string    `json:"checkpoint_sha256"`
	CheckpointPost string    `json:"checkpoint_post"`
	Stage          int       `json:"stage"`
	Position       int64     `json:"position"`
	Receiver       string    `json:"old_receiver_sha256"`
	Audit          string    `json:"audit_sha256"`
	FirstID        int64     `json:"audit_first_id"`
	LastID         int64     `json:"audit_last_id"`
	Events         int64     `json:"audit_events"`
	Allows         int       `json:"assignment_allows"`
	LaterAllows    int       `json:"allows_after_restore"`
	RestoreFirst   time.Time `json:"first_restore_at"`
	RestoreSecond  time.Time `json:"second_restore_at"`
	CRM            time.Time `json:"first_crm_at"`
	Revoked        time.Time `json:"revoked_at"`
	Approved       time.Time `json:"approved_at"`
	Expires        time.Time `json:"expires_at"`
}

func (a RecoveryApproval) Digest() string {
	b, _ := json.Marshal(a)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Registration is stable when a historical approval is retired or expires.
func (a RecoveryApproval) Registration() string {
	b, _ := json.Marshal([]any{"orpheus.mattermost.report-storage.v1", a.Job, a.Card, a.Journal, a.Device, a.Inode})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (a RecoveryApproval) Valid(c Card) bool {
	return a.Schema == "orpheus.mattermost.report-recovery.v1" && a.Job == c.Job && a.Card == c.Digest() &&
		GUID(a.Journal) && a.Device != 0 && a.Inode != 0 && GUID(a.Run) && GUID(a.Session) && GUID(a.Invocation) &&
		sha.MatchString(a.Assignment) && id.MatchString(a.Original) && id.MatchString(a.Upgrade) && a.Original != a.Upgrade &&
		sha.MatchString(a.Method) && sha.MatchString(a.Checkpoint) && id.MatchString(a.CheckpointPost) && a.Position > 0 && a.Stage >= 2 &&
		a.Receiver == AuditedReceiverSHA && sha.MatchString(a.Audit) && a.FirstID > 0 && a.LastID >= a.FirstID && a.Events >= 5 &&
		a.LastID-a.FirstID+1 == a.Events && a.Allows == 2 && a.LaterAllows == 0 &&
		!a.RestoreFirst.IsZero() && a.RestoreFirst.Before(a.RestoreSecond) && a.RestoreSecond.Before(a.CRM) && a.CRM.Before(a.Revoked) &&
		!a.Approved.Before(a.Revoked) && a.Expires.After(a.Approved) && a.Expires.Sub(a.Approved) <= 24*time.Hour
}

func (a RecoveryApproval) registration(c Card) bool {
	return a.Job == c.Job && a.Card == c.Digest() && GUID(a.Journal) && a.Device != 0 && a.Inode != 0
}

type Publication struct {
	Approval RecoveryApproval
	File     *os.File
}

func OpenPublication(card Card) (*Publication, error) {
	raw, err := readMethodApproval(os.Getenv("MM_REPORT_RECOVERY_APPROVAL"))
	if err != nil {
		return nil, err
	}
	var a RecoveryApproval
	if json.Unmarshal(raw, &a) != nil || !a.registration(card) {
		return nil, errors.New("invalid report publication control")
	}
	fields := "schema job_id card_sha256 journal_id device inode"
	switch a.Schema {
	case "orpheus.mattermost.report-journal.v1":
	case "orpheus.mattermost.report-recovery.v1":
		if !a.Valid(card) {
			return nil, errors.New("invalid historical report approval")
		}
		fields += " run_id session_id invocation_id assignment_sha256 original_post upgrade_post method_sha256 checkpoint_sha256 checkpoint_post stage position old_receiver_sha256 audit_sha256 audit_first_id audit_last_id audit_events assignment_allows allows_after_restore first_restore_at second_restore_at first_crm_at revoked_at approved_at expires_at"
	default:
		return nil, errors.New("invalid report publication control")
	}
	if !exactJSONFields(raw, strings.Fields(fields)...) {
		return nil, errors.New("invalid report publication control fields")
	}
	path := os.Getenv("MM_REPORT_PUBLICATION_JOURNAL")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid journal path")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0660 || !trustedJournalFile(f, st, a) {
		_ = f.Close()
		return nil, errors.New("untrusted publication journal")
	}
	return &Publication{Approval: a, File: f}, nil
}

type PublicationEvent struct {
	Stage      int    `json:"stage"`
	Run        string `json:"run"`
	Invocation string `json:"invocation"`
	Action     string `json:"action"`
	Digest     string `json:"digest"`
}

type publicationState struct {
	opens     map[string]string
	result    PublicationEvent
	last      string
	claim     string
	handedOff bool
}

func (s *publicationState) apply(v PublicationEvent) (bool, error) {
	if v.Stage < 1 || !GUID(v.Run) || !GUID(v.Invocation) || !sha.MatchString(v.Digest) {
		return false, errors.New("invalid publication event")
	}
	conflict := errors.New("report publication already attempted or not covered")
	switch v.Action {
	case "claim":
		if s.claim != "" || s.last != "" || len(s.opens) > 0 {
			return false, conflict
		}
		s.claim = v.Invocation
	case "handoff":
		if s.claim == "" || s.handedOff || s.claim == v.Invocation || s.last != "" ||
			s.opens[v.Run] != s.claim+" "+v.Digest {
			return false, conflict
		}
		s.claim = v.Invocation
		s.handedOff = true
	case "open":
		if s.claim != "" && s.claim != v.Invocation {
			return false, conflict
		}
		key := v.Invocation + " " + v.Digest
		if previous, ok := s.opens[v.Run]; ok {
			if previous != key {
				return false, conflict
			}
			return false, nil
		}
		if s.last != "" {
			return false, conflict
		}
		s.opens[v.Run] = key
	case "result":
		if s.claim != "" && s.claim != v.Invocation || !strings.HasPrefix(s.opens[v.Run], v.Invocation+" ") || s.last != "" {
			return false, conflict
		}
		s.result = v
		s.last = v.Action
	case "upload", "publish", "receipt":
		if s.result.Run != v.Run || s.result.Invocation != v.Invocation || s.result.Digest != v.Digest {
			return false, conflict
		}
		if v.Action == "receipt" && s.last == "receipt" {
			return false, nil
		}
		required := map[string]string{"upload": "result", "publish": "upload", "receipt": "publish"}
		if s.last != required[v.Action] {
			return false, conflict
		}
		s.last = v.Action
	default:
		return false, errors.New("unknown publication event")
	}
	return true, nil
}

func (p *Publication) Record(ctx context.Context, v PublicationEvent) error {
	return p.visit(ctx, v, true)
}

// Before a new admission, reject a stage carrying any unsettled result
// intent, even if an inconsistent hook claims it was safe to run again.
func (p *Publication) CheckUnsubmitted(ctx context.Context, v PublicationEvent) error {
	return p.visit(ctx, v, false)
}

func (p *Publication) visit(ctx context.Context, v PublicationEvent, write bool) error {
	f := p.File
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	const maxJournal = 32 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxJournal+1))
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(data) > maxJournal || len(lines) < 2 || len(lines[len(lines)-1]) != 0 ||
		string(lines[0]) != journalHeader+p.Approval.Journal+" "+p.Approval.Job+" "+p.Approval.Card {
		return errors.New("incomplete publication journal")
	}
	states := map[int]*publicationState{}
	state := func(stage int) *publicationState {
		s := states[stage]
		if s == nil {
			s = &publicationState{opens: make(map[string]string)}
			states[stage] = s
		}
		return s
	}
	for _, line := range lines[1 : len(lines)-1] {
		var old PublicationEvent
		if len(line) > 1024 || !exactJSONFields(line, "stage", "run", "invocation", "action", "digest") || json.Unmarshal(line, &old) != nil {
			return errors.New("corrupt publication journal")
		}
		add, err := state(old.Stage).apply(old)
		if err != nil || !add {
			return errors.New("inconsistent publication journal")
		}
	}
	current := state(v.Stage)
	if !write {
		if current.last != "" || current.claim != "" && current.claim != v.Invocation {
			return errors.New("unsettled report publication")
		}
		if opened, ok := current.opens[v.Run]; ok && opened != v.Invocation+" "+v.Digest {
			return errors.New("report opening changed")
		}
		return ctx.Err()
	}
	add, err := current.apply(v)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !add {
		return f.Sync()
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(data)+len(raw) > maxJournal {
		return errors.New("publication journal full")
	}
	n, err := f.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("publication persistence: %w", err)
	}
	return nil
}
