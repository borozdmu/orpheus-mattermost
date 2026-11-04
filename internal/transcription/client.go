// Package transcription implements bounded file transcription over the
// OpenAI-compatible multipart API. It never retries an external request.
package transcription

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxTextBytes = 64 << 10

var ErrRequest = errors.New("voice transcription unavailable; request was not retried")

type Client struct {
	endpoint, model, key string
	http                 *http.Client
}

func New(endpoint, model, key string, timeout time.Duration) *Client {
	return &Client{endpoint: endpoint, model: model, key: key, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Transcribe(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 || len(data) > 20_000_000 {
		return "", ErrRequest
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if w.WriteField("model", c.model) != nil || w.WriteField("response_format", "json") != nil {
		return "", ErrRequest
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="audio.input"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	if err != nil {
		return "", ErrRequest
	}
	if _, err = part.Write(data); err != nil {
		return "", ErrRequest
	}
	if w.Close() != nil {
		return "", ErrRequest
	}
	// A non-replayable body also prevents net/http from replaying a POST.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, struct{ io.Reader }{&body})
	if err != nil {
		return "", ErrRequest
	}
	req.ContentLength = int64(body.Len())
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", w.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		return "", ErrRequest
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", ErrRequest
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (256<<10)+1))
	var result struct {
		Text string `json:"text"`
	}
	if err != nil || len(raw) > 256<<10 || json.Unmarshal(raw, &result) != nil {
		return "", ErrRequest
	}
	text := strings.TrimSpace(result.Text)
	if text == "" || len(text) > MaxTextBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", ErrRequest
	}
	return text, nil
}
