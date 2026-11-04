package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

// The local processor owns extraction and masking. The sandbox receives only
// approved text. Never give it original attachments or a Mattermost credential.
func projectLocalInput(ctx context.Context, p *pending, mm MM) error {
	source, ok := mm.(attachments.InputSource)
	if !ok {
		return errors.New("local file source unavailable")
	}
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/run/orpheus-input/input.sock")
	}}
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	transform := func(mime string, b []byte) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/project", bytes.NewReader(b))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", mime)
		res, err := client.Do(req)
		if err != nil {
			return "", errors.New("local input processor unavailable")
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != 200 {
			return "", errors.New("local input rejected")
		}
		out, err := io.ReadAll(io.LimitReader(res.Body, 512*1024+1))
		if err != nil || len(out) > 512*1024 || !utf8.Valid(out) {
			return "", errors.New("invalid local projection")
		}
		return string(out), nil
	}
	messages := make([]conversation.InputMessage, 0, len(p.Messages))
	total := int64(0)
	for _, item := range p.Messages {
		post, err := mm.Post(ctx, item.PostID)
		if err != nil || post.DeleteAt != 0 || post.Root() != p.Envelope.Root || post.ChannelID != p.Envelope.Channel || post.UserID != item.Subject.ID || !slices.Contains(p.Envelope.Versions, conversation.PostVersion(post)) || !slices.Contains(p.Envelope.TriggerIDs, post.ID) {
			return errors.New("current input changed or crosses channel boundary")
		}
		var body strings.Builder
		if post.Message != "" {
			s, err := transform("text/plain", []byte(post.Message))
			if err != nil {
				return err
			}
			body.WriteString(s)
		}
		for _, id := range post.FileIDs {
			i := slices.IndexFunc(p.Envelope.Request.Files, func(f attachments.InputFile) bool {
				return f.FileID == id && f.PostID == post.ID && f.ChannelID == post.ChannelID
			})
			if i < 0 || p.Envelope.Request.Files[i].Status != "" {
				return errors.New("unverified attachment")
			}
			f, b := attachments.Fetch(ctx, source, p.Envelope.Request, p.Envelope.Request.Files[i])
			total += int64(len(b))
			if f.Status != "ready" || total > p.Envelope.Request.Limits.MaxBatchBytes {
				return errors.New("local attachment unavailable")
			}
			mime, _, _ := strings.Cut(f.MIME, ";")
			s, err := transform(mime, b)
			if err != nil {
				return err
			}
			body.WriteString("\n\nСодержимое вложения:\n")
			body.WriteString(s)
		}
		item.Text = body.String()
		messages = append(messages, item)
	}
	p.Messages = messages
	p.Envelope.Request.Files = nil
	return nil
}
