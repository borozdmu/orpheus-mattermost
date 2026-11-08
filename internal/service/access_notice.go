package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type audienceDenial struct {
	code string
	ids  []string
}

func (d *audienceDenial) Error() string { return strings.ReplaceAll(d.code, "_", " ") }

var publicUsername = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var errWithholdOutput = errors.New("original output authority cannot be confirmed")
var errNoticeChanged = errors.New("channel membership or admission decision changed before notice")

// denialText reads only verified channel members. Identifiers from the downstream service,
// email, profile text, and request or result content never enter the notice.
func denialText(d *audienceDenial, usernames map[string]string, service string) (string, error) {
	switch d.code {
	case "channel_policy_unavailable":
		return "Сейчас не удалось проверить права участников этого чата. Попробуйте отправить запрос позже.", nil
	case "service_access_denied", "input_subject_denied":
		if service == "" {
			service = "Сервис данных"
		}
		return service + " не подтвердил доступ для этого чата. Служба не указала, для какого участника. Попросите администратора проверить привязки и права, затем повторите запрос.", nil
	case "channel_policy_denied":
		return "Правила доступа этого чата не разрешают выполнить запрос. Проверьте права у администратора и повторите запрос.", nil
	case "identity_unmapped", "identity_unconfirmed", "participant_access_unconfirmed":
	default:
		return "", errors.New("unsupported admission notice")
	}
	names := make([]string, 0, len(d.ids))
	for _, id := range d.ids {
		name := "участник без доступного имени"
		if publicUsername.MatchString(usernames[id]) {
			name = "`" + usernames[id] + "`"
		}
		names = append(names, name)
	}
	if d.code == "participant_access_unconfirmed" {
		if service == "" {
			service = "Сервис данных"
		}
		return service + " не подтвердил доступ для следующих участников чата: " + strings.Join(names, ", ") + ". Попросите администратора проверить привязку учётной записи и доступ к этому агенту, затем повторите запрос.", nil
	}
	account := "учётной записью сервиса данных"
	if service != "" {
		account = "учётной записью " + service
	}
	start := "Не удалось подтвердить связь с " + account + " у следующих участников чата: "
	if d.code == "identity_unmapped" {
		start = "Для следующих участников чата не настроена связь с " + account + ": "
	}
	return start + strings.Join(names, ", ") + ". Попросите администратора проверить привязку учётных записей, затем отправьте запрос ещё раз.", nil
}

// A control notice is tied to the exact request and contains no agent output. Re-observe
// the channel and the decision immediately before each newly created post, including retries.
func (e *Engine) accessNotice(ctx context.Context, w config.Workflow, p *delivery.Publisher, env conversation.Envelope, actor string, authors []string, failure error, outputRun string) error {
	denied, ok := errors.AsType[*audienceDenial](failure)
	if !ok {
		return failure
	}
	if !e.noticeDue(w, p, env) {
		return nil
	}
	observe := func(ctx context.Context) ([]string, string, error) {
		usernames := make(map[string]string, len(denied.ids))
		for _, id := range denied.ids {
			usernames[id] = ""
		}
		aud, _, err := e.readAudience(ctx, w, p.Key, usernames)
		if err != nil {
			return nil, "", err
		}
		for _, who := range authors {
			if !slices.Contains(aud, who) || who == e.Bot.ID || w.Access.Peer != nil && who == w.Access.Peer.ID {
				return nil, "", errNoticeChanged
			}
		}
		_, _, decision := projectAccess(*w.Access, aud, actor)
		if decision == nil && w.Access.LocalPolicy() {
			decision = localGate(ctx, w, env, aud, authors)
		}
		// A definitive core refusal has no person-level details. Checking it by
		// submitting the request again could start a run, so only recheck the local audience.
		if decision == nil && (denied.code == "service_access_denied" || denied.code == "input_subject_denied") {
			decision = denied
		}
		now, ok := errors.AsType[*audienceDenial](decision)
		if !ok || denied.code != now.code || !slices.Equal(denied.ids, now.ids) {
			return nil, "", errNoticeChanged
		}
		text, err := denialText(now, usernames, e.Config.AccessNoticeService)
		return aud, text, err
	}
	aud, text, err := observe(ctx)
	if err != nil {
		return err
	}
	safe := *p
	safe.BeforeCreate = func(ctx context.Context) error {
		current, msg, err := observe(ctx)
		if err != nil {
			return err
		}
		if !slices.Equal(aud, current) || msg != text {
			return errNoticeChanged
		}
		return nil
	}
	kind := "input_unmapped"
	run := env.Anchor
	if outputRun != "" {
		kind = "output_access_notice"
		run = outputRun
	}
	// Multiple fragments share one receipt group. Only the final fragment marks the
	// input rejected, so a failure between fragments can resume after a restart.
	group := run
	if kind == "input_unmapped" {
		group += ":" + attachments.Hash([]byte(env.Revision+"\n"+strings.Join(env.TriggerIDs, "\n")+"\n"+text+"\n"+noticeVersions(env)))
	}
	for _, part := range delivery.Parts(p.Key, "", group, kind, text, env.Render, nil) {
		if kind == "input_unmapped" && part.Receipt.Part == part.Receipt.Parts-1 {
			part.Receipt.TriggerIDs = env.TriggerIDs
			for _, v := range env.Versions {
				if slices.Contains(env.TriggerIDs, v.ID) {
					part.Receipt.InputVersions = append(part.Receipt.InputVersions, v)
				}
			}
		}
		if err := safe.Publish(ctx, part); err != nil {
			return err
		}
	}
	p.Posts = safe.Posts
	return nil
}

func noticeVersions(env conversation.Envelope) string {
	versions := slices.DeleteFunc(slices.Clone(env.Versions), func(v conversation.Version) bool { return !slices.Contains(env.TriggerIDs, v.ID) })
	b, _ := json.Marshal(versions)
	return string(b)
}

func triggerAuthors(p pending) (string, []string) {
	var actor string
	var authors []string
	for _, post := range p.TriggerPosts {
		if post.ID == p.Envelope.Anchor {
			actor = post.UserID
		}
		if !slices.Contains(authors, post.UserID) {
			authors = append(authors, post.UserID)
		}
	}
	return actor, authors
}

func (e *Engine) noticeStart(w config.Workflow) time.Time {
	if !e.Config.AccessNoticesFrom.IsZero() {
		return e.Config.AccessNoticesFrom
	}
	return w.Since
}
func (e *Engine) noticeDue(w config.Workflow, p *delivery.Publisher, env conversation.Envelope) bool {
	return slices.ContainsFunc(p.Posts, func(post mattermost.Post) bool {
		return post.ChannelID == p.Key.Channel && post.Root() == p.Key.Root && post.DeleteAt == 0 && slices.Contains(env.TriggerIDs, post.ID) && !time.UnixMilli(post.CreateAt).Before(e.noticeStart(w))
	})
}
