package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/transcription"
)

// Audio stays attached for ordinary workflows. The same-author transcript is
// frozen with the input before admission; local-policy workflows never use this.
func projectVoiceInput(ctx context.Context, p *pending, mm MM, voice VoiceTranscriber, limit int) error {
	messages := slices.Clone(p.Messages)
	source, ok := mm.(attachments.InputSource)
	total := int64(0)
	for _, input := range p.Envelope.Request.Files {
		mime, _, _ := strings.Cut(input.MIME, ";")
		if !strings.HasPrefix(mime, "audio/") {
			continue
		}
		if !ok || voice == nil || input.Status != "" || input.Size > 20_000_000 {
			return errors.New("audio source or transcription unavailable")
		}
		post, err := source.Post(ctx, input.PostID)
		if err != nil || post.DeleteAt != 0 || post.ChannelID != input.ChannelID || !slices.Contains(p.Envelope.Versions, conversation.PostVersion(post)) {
			return errors.New("audio source changed")
		}
		idx := slices.IndexFunc(messages, func(m conversation.InputMessage) bool { return m.PostID == input.PostID })
		if idx < 0 || (p.Workflow.Access != nil && messages[idx].Subject.ID != post.UserID) {
			return errors.New("audio author not confirmed")
		}
		request := p.Envelope.Request
		request.Limits.MaxFileBytes = min(request.Limits.MaxFileBytes, 20_000_000)
		file, data := attachments.Fetch(ctx, source, request, input)
		total += int64(len(data))
		if file.Status != "ready" || file.MIME != input.MIME || total > request.Limits.MaxBatchBytes {
			return errors.New("audio attachment unavailable")
		}
		text, err := voice.Transcribe(ctx, data)
		if err != nil {
			return err
		}
		if text == "" || len(text) > transcription.MaxTextBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return errors.New("invalid voice transcript")
		}
		messages[idx].Text += fmt.Sprintf("\n\nРасшифровка аудиовложения %q:\n%s\n", attachments.SafeName(input.Name), text)
	}
	raw, err := json.Marshal(struct {
		Messages []conversation.InputMessage
		Metadata conversation.Envelope
	}{messages, p.Envelope})
	if err != nil || len(raw) >= limit*3/4 {
		return errors.New("transcribed input exceeds request budget")
	}
	p.Messages = messages
	return nil
}
