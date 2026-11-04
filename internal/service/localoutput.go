package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

var localArtifactPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var localNamePattern = regexp.MustCompile(`^document-[0-9]{2}\.text-version\.(txt|docx|xlsx|pdf)$`)

type localOutputSource interface {
	Upload(context.Context, string, string, io.Reader) (mattermost.FileInfo, error)
}

type localManifest struct {
	Schema    int                    `json:"schema"`
	SessionID string                 `json:"session_id"`
	RunID     string                 `json:"run_id"`
	Stage     string                 `json:"stage"`
	Files     []attachments.Artifact `json:"files"`
}

// localOutput consumes only a sealed, locally approved derivative. The model has
// neither the Mattermost token nor a file-upload capability for that channel.
func (e *Engine) localOutput(ctx context.Context, p *delivery.Publisher, s conversation.Session, r conversation.Run, env conversation.Envelope) (attachments.Output, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/run/orpheus-output/output.sock")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e.projectLocalOutput(ctx, p, s, r, env, client)
}

func (e *Engine) projectLocalOutput(ctx context.Context, p *delivery.Publisher, s conversation.Session, r conversation.Run, env conversation.Envelope, client *http.Client) (attachments.Output, error) {
	output := attachments.Output{Files: []attachments.Artifact{}}
	upload, ok := e.MM.(localOutputSource)
	if !ok || p.BeforeCreate == nil {
		return output, errors.New("local output transport unavailable")
	}
	completed := false
	for _, hook := range r.Hooks {
		if hook.Name == "after_run" && hook.Status == "completed" && hook.Completeness == "complete" {
			completed = true
		}
	}
	if !completed {
		return output, errors.New("local output export incomplete")
	}
	request := func(operation, artifact string, limit int64) ([]byte, error) {
		body, err := json.Marshal(map[string]string{"session_id": s.ID, "run_id": r.ID, "artifact_id": artifact})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local"+operation, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("local output unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("local output rejected")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, errors.New("local output exceeds limit")
		}
		return data, nil
	}
	raw, err := request("/manifest", "", 16<<10)
	if err != nil {
		return output, err
	}
	var manifest localManifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.Schema != 1 || manifest.SessionID != s.ID || manifest.RunID != r.ID || manifest.Stage != "sealed" || len(manifest.Files) > min(env.Request.Limits.MaxOutputFiles, 15) {
		return output, errors.New("invalid local manifest")
	}
	seen := map[string]bool{}
	for _, artifact := range manifest.Files {
		if !localArtifactPattern.MatchString(artifact.ArtifactID) || !localArtifactPattern.MatchString(artifact.SHA256) || !localNamePattern.MatchString(artifact.Name) || artifact.FileID != "" || artifact.Path != "" || seen[artifact.ArtifactID] || artifact.Size < 0 || artifact.Size > min(env.Request.Limits.MaxOutputBytes, 4<<20) {
			return output, errors.New("invalid local artifact")
		}
		seen[artifact.ArtifactID] = true
	}
	// Validate every descriptor before uploading any file; publication remains all-or-nothing.
	for _, artifact := range manifest.Files {
		existing, found, err := p.ExistingArtifact(ctx, s.ID, r.ID, artifact.ArtifactID, artifact.SHA256)
		if err != nil {
			return output, err
		}
		if found {
			output.Files = append(output.Files, existing)
			continue
		}
		data, err := request("/read", artifact.ArtifactID, artifact.Size)
		if err != nil || int64(len(data)) != artifact.Size || attachments.Hash(data) != artifact.SHA256 {
			return output, errors.New("local artifact integrity failed")
		}
		// Uploading is a disclosure too. Recheck the exact audience before both the
		// file upload and the subsequent post creation.
		if err = p.BeforeCreate(ctx); err != nil {
			return output, err
		}
		info, err := upload.Upload(ctx, env.Channel, artifact.Name, bytes.NewReader(data))
		if err != nil {
			return output, err
		}
		if info.ID == "" || info.PostID != "" {
			return output, errors.New("invalid local upload receipt")
		}
		artifact.FileID = info.ID
		artifact.Path = "snapshot/" + artifact.ArtifactID + "/" + artifact.Name
		output.Files = append(output.Files, artifact)
	}
	return output, nil
}
