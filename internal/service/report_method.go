package service

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

func reportRank(s *reportjob.Stage) [3]int {
	method := 0
	if s.Method != nil {
		method = 1
		if s.Method.Previous != nil {
			method = 2
		}
	}
	return [3]int{s.Number, method, s.Attempt}
}

func laterReport(a, b [3]int) bool { return slices.Compare(a[:], b[:]) > 0 }

func reservedMethodCommand(text, username string) bool {
	words := strings.Fields(text)
	return len(words) >= 2 && words[0] == "@"+username && (words[1] == "/report-upgrade" || words[1] == "/report-upgrade-from-checkpoint")
}

var loadSecondApproval = reportjob.LoadSecondApproval

func approvedMethod(s reportjob.Stage, card reportjob.Card, user string) (name, command string, err error) {
	a, err := loadMethodApproval(card)
	if err != nil {
		return "", "", err
	}
	if s.Method.Previous == nil {
		if !s.Method.Approved(a, card) {
			return "", "", errReportBinding
		}
		return a.FileName, a.Command(user), nil
	}
	next, err := loadSecondApproval(card, a)
	if err != nil || !s.ApprovedSecond(card, a, next) {
		return "", "", errReportBinding
	}
	return next.FileName, next.Command(user), nil
}

func reportMethodInputBound(env conversation.Envelope, card reportjob.Card) bool {
	if env.Report == nil || env.Report.Method == nil {
		return true
	}
	u := env.Report.Method
	name, _, err := approvedMethod(*env.Report, card, "")
	if err != nil || env.Anchor != u.Post || len(env.Request.Files) != 1 || env.Request.Files[0].FileID != u.File || env.Request.Files[0].Name != name {
		return false
	}
	return slices.Contains(env.Versions, conversation.Version{ID: u.Post, Hash: u.PostHash})
}

// reportCommand checks both actual native commands for a replacement. File
// delivery, notice creation and every subsequent stage recall these bindings.
func (e *Engine) reportCommand(ctx context.Context, card reportjob.Card, env conversation.Envelope) (mattermost.Post, error) {
	var empty mattermost.Post
	post, err := e.MM.Post(ctx, env.Anchor)
	if err != nil {
		return empty, err
	}
	valid := func(p mattermost.Post) bool {
		return p.UserID == card.Actor && p.Root() == card.Thread && p.ChannelID == card.Channel && p.DeleteAt == 0
	}
	if post.ID != env.Anchor || !valid(post) || !slices.Contains(env.Versions, conversation.PostVersion(post)) {
		return empty, errReportOrigin
	}
	u := env.Report.Method
	if u == nil {
		if !reportjob.MatchesCommand(post.Message, e.Bot.Username, card) {
			return empty, errReportOrigin
		}
		return post, nil
	}
	current := post
	stage := *env.Report
	for u != nil {
		stage.Method = u
		name, command, err := approvedMethod(stage, card, e.Bot.Username)
		if err != nil || current.EditAt != 0 || current.ID != u.Post || conversation.PostVersion(current).Hash != u.PostHash ||
			len(current.FileIDs) != 1 || current.FileIDs[0] != u.File || strings.TrimSpace(current.Message) != command {
			return empty, errReportOrigin
		}
		info, err := e.MM.File(ctx, u.File)
		if err != nil {
			return empty, err
		}
		if info.ID != u.File || info.UserID != card.Actor || info.PostID != u.Post || info.Name != name {
			return empty, errReportOrigin
		}
		before, err := e.MM.Post(ctx, u.Original)
		if err != nil {
			return empty, err
		}
		v := conversation.PostVersion(before)
		if !valid(before) || v.ID != u.Original || v.Hash != u.OriginalHash || v.EditedAt != u.OriginalEdited || current.CreateAt <= before.CreateAt {
			return empty, errReportOrigin
		}
		current, u = before, u.Previous
	}
	if !reportjob.MatchesCommand(current.Message, e.Bot.Username, card) {
		return empty, errReportOrigin
	}
	return post, nil
}

func (e *Engine) requestedMethod(card reportjob.Card, prev conversation.Envelope, original mattermost.Post, posts []mattermost.Post) (*reportjob.MethodRevision, error) {
	prior := prev.Report.Method
	if prior != nil && (prior.Previous != nil || prev.Report.Attempt != 3 || prev.Report.Diagnostic || prev.Report.Storage == "" || prev.Report.Prior == nil) {
		return nil, nil
	}
	var selected *reportjob.MethodRevision
	for _, post := range posts {
		if post.UserID != card.Actor || post.Root() != card.Thread || post.ChannelID != card.Channel || post.DeleteAt != 0 || !reservedMethodCommand(post.Message, e.Bot.Username) {
			continue
		}
		a, err := loadMethodApproval(card)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		command, from, to, digest := a.Command(e.Bot.Username), a.From, a.To, a.Digest()
		if prior != nil {
			if strings.Fields(post.Message)[1] != "/report-upgrade-from-checkpoint" {
				continue
			}
			next, err := loadSecondApproval(card, a)
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			if err != nil || !next.Valid(card, a) || !prior.Approved(a, card) ||
				next.Stage != prev.Report.Number || next.Position != prev.Report.Prior.Position || next.Checkpoint != prev.Report.Prior.SHA {
				return nil, errReportBinding
			}
			command, from, to, digest = next.Command(e.Bot.Username), next.From, next.To, next.Digest()
		}
		if strings.TrimSpace(post.Message) != command {
			continue
		}
		if selected != nil || post.ID == original.ID || post.CreateAt <= original.CreateAt || post.EditAt != 0 || len(post.FileIDs) != 1 {
			return nil, errors.New("report method command is ambiguous or changed")
		}
		before, after := conversation.PostVersion(original), conversation.PostVersion(post)
		selected = &reportjob.MethodRevision{
			Original: before.ID, OriginalHash: before.Hash, OriginalEdited: before.EditedAt,
			Post: after.ID, PostHash: after.Hash, File: post.FileIDs[0],
			From: from, To: to, Approval: digest, Previous: prior,
		}
	}
	return selected, nil
}

func sameReportLineage(source, dst conversation.Envelope) bool {
	u, prior := dst.Report.Method, source.Report.Method
	if u == nil {
		return prior == nil && source.Anchor == dst.Anchor
	}
	if dst.Report.Number == u.First {
		return (prior == nil && u.Previous == nil || prior != nil && u.Previous != nil && prior.Match(*u.Previous)) && srcWasOriginal(source, u)
	}
	return prior != nil && prior.Match(*u) && source.Anchor == dst.Anchor
}

func srcWasOriginal(source conversation.Envelope, u *reportjob.MethodRevision) bool {
	return source.Anchor == u.Original && slices.Contains(source.Versions,
		conversation.Version{ID: u.Original, EditedAt: u.OriginalEdited, Hash: u.OriginalHash})
}

func provenBeforeSubmission(env conversation.Envelope, run conversation.Run) bool {
	if reportHookWitness(env, run, "attempted-submission", "report-submission-v1") {
		return false
	}
	if legacyCalculationFailure(env, run) || reportHookWitness(env, run, "retry-before-upload", "report-retry-v1") {
		return true
	}
	if !env.Report.Diagnostic || run.Status != "failed" || run.CancelRequested || run.FinishedAt == nil || len(run.Hooks) != 1 {
		return false
	}
	h := run.Hooks[0]
	if h.Name != "before_run" || h.Status != "failed" || h.Completeness != "complete" || h.StartedAt == nil ||
		h.DeadlineAt == nil || h.FinishedAt == nil || !h.FinishedAt.After(*h.StartedAt) || !h.FinishedAt.Before(*h.DeadlineAt) {
		return false
	}
	code, rest, ok := strings.Cut(h.Output, "\n")
	if !ok || rest != "Mattermost file hook failed: stage=report_calculation; validation failed: report calculation did not finish this stage\n" {
		return false
	}
	name, ok := strings.CutPrefix(code, "report-calculation-v1 ")
	return ok && slices.Contains(strings.Fields(legacyCalculationReasons), name)
}

// Only report-calculation-v1 labels actually emitted by the previous receiver
// prove an unsubmitted failure. New diagnostic formats require separate review.
const legacyCalculationReasons = `scoped_source_changed unresolved_history_order_site site_code_mismatch wrong_shop
live_read_grant_unconfirmed source_not_granted changed_method_or_dictionary wrong_previous_assignment previous_file_sha_mismatch
previous_stage_mismatch unproven_insertion_order_at_source_boundary new_history_inside_closed_reporting_period invalid_alignment
incomplete_page missing_history_range unconfirmed_custom_value marker_size_rejected incorrect_history_field program_error
source_validation history_alignment_changed history_count_changed history_order_changed history_cursor_changed
history_final_count_changed history_boundary_changed required_key_missing invalid_value invalid_type io_error memory_error`
