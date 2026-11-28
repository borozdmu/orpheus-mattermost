package orpheus

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

var ErrSourceReadDenied = errors.New("source read was not authorized")
var ErrSourceReadUnavailable = errors.New("source read service is temporarily unavailable")

// SourceRead identifies a single stage of an owner-confirmed job. The server
// resolves the signed assignment's route and currently active invocation.
type SourceRead struct {
	Invocation string `json:"invocation_id"`
	Assignment string `json:"assignment_digest"`
	Job        string `json:"job_id"`
	SHA        string `json:"artifact_sha256"`
	Source     string `json:"source_id"`
	Revision   string `json:"source_revision"`
	Purpose    string `json:"purpose"`
}

func (c *Client) CheckSourceRead(ctx context.Context, req SourceRead, secret string) error {
	if secret == "" || req.Purpose != "restore" && req.Purpose != "upload" && req.Purpose != "publish" {
		return errors.New("invalid source-read proof")
	}
	id := uuid.NewString()
	material, _ := json.Marshal([]string{"orpheus.source-read.v1", id, req.Invocation, req.Assignment, req.Job, req.SHA, req.Source, req.Revision, req.Purpose})
	signature := hmac.New(sha256.New, []byte(secret))
	_, _ = signature.Write(material)
	body, err := json.Marshal(struct {
		SourceRead
		ID        string `json:"request_id"`
		Signature string `json:"signature"`
	}{req, id, hex.EncodeToString(signature.Sum(nil))})
	if err != nil {
		return err
	}
	addr, err := url.JoinPath(c.api.Server, "api/v1/agent-access/source-read")
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	for _, edit := range c.api.RequestEditors {
		if err = edit(ctx, r); err != nil {
			return err
		}
	}
	// Source authorization observes both audiences and may need the complete
	// protected-operation budget. Keep the ordinary API client's transport and
	// redirect policy, without changing its shorter timeout for unrelated calls.
	client, ok := c.api.Client.(*http.Client)
	if !ok {
		return errors.New("unsupported source authorizer transport")
	}
	h := *client
	h.Timeout = 150 * time.Second
	res, err := h.Do(r)
	if err != nil {
		return ErrSourceReadUnavailable
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == 429 || res.StatusCode >= 500 {
		return ErrSourceReadUnavailable
	}
	if res.StatusCode != http.StatusOK {
		return ErrSourceReadDenied
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4097))
	if err != nil {
		return ErrSourceReadUnavailable
	}
	var response struct {
		ID         string `json:"request_id"`
		Invocation string `json:"invocation_id"`
		Assignment string `json:"assignment_digest"`
		Digest     string `json:"material_sha256"`
		Allowed    bool   `json:"allowed"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 4096 || !jsontext.Value(raw).IsValid() || decoder.Decode(&response) != nil {
		return ErrSourceReadDenied
	}
	sum := sha256.Sum256(material)
	if err != nil || !response.Allowed || response.ID != id || response.Invocation != req.Invocation || response.Assignment != req.Assignment || response.Digest != hex.EncodeToString(sum[:]) {
		return ErrSourceReadDenied
	}
	return nil
}
