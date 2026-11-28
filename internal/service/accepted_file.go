package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

// CheckAcceptedFile reads exactly one accepted trigger attachment using the
// configured receiver. Only the byte count may leave this diagnostic.
func (e *Engine) CheckAcceptedFile(ctx context.Context, w config.Workflow, key conversation.Key, runID, postID, fileID, sha string) (int64, error) {
	run, err := uuid.Parse(runID)
	hash, hashErr := hex.DecodeString(sha)
	if err != nil || run == uuid.Nil || run.String() != runID || hashErr != nil || len(hash) != 32 || !config.ValidID(postID) || !config.ValidID(fileID) ||
		!config.ValidID(key.Channel) || !config.ValidID(key.Root) || w.ID != key.Workflow || w.Access == nil || w.Access.LocalPolicy() || w.Draining {
		return 0, errors.New("invalid accepted file selection")
	}
	bot, err := e.MM.Me(ctx)
	if err != nil || bot.Username == "" || bot.DeleteAt != 0 || bot.ID != w.Access.BotID || key.Source != w.Mattermost.Source(bot.ID) {
		return 0, errors.New("attachment receiver identity unconfirmed")
	}
	snapshot, err := e.API.Snapshot(ctx, key)
	if err != nil {
		return 0, errors.New("accepted input history unavailable")
	}
	for _, s := range snapshot.Sessions {
		if s.ExternalKey != key.External() {
			continue
		}
		for _, r := range s.Runs {
			if r.ID != runID || r.SessionID != s.ID {
				continue
			}
			delivered := slices.ContainsFunc(s.Messages, func(m conversation.Message) bool {
				return m.RunID == runID && m.Role == "user" && m.Delivery == "delivered" &&
					m.ExternalKey == key.MessageKey(postID) && conversation.HasMetadata(m.Metadata)
			})
			if !delivered {
				return 0, errors.New("accepted trigger not delivered")
			}
			env, err := protectedContract(&s, &r, w, key)
			if err != nil || env.Anchor != postID || env.Request.BotID != bot.ID || env.Request.AnchorID != postID ||
				env.Request.RootID != key.Root || env.Request.ChannelID != key.Channel || env.Request.SourceID != key.Source || env.Request.Validate() != nil {
				return 0, errors.New("accepted trigger binding changed")
			}
			i := slices.IndexFunc(env.Request.Files, func(f attachments.InputFile) bool {
				return f.FileID == fileID && f.PostID == postID && f.ChannelID == key.Channel && f.Origin == "thread" && f.Status == ""
			})
			if i < 0 {
				return 0, errors.New("file absent from accepted trigger")
			}
			observer := Engine{MM: e.MM, API: e.API, Config: e.Config, Bot: bot}
			data, err := observer.readBoundFile(ctx, w, env, env.Request.Files[i], sha)
			return int64(len(data)), err
		}
	}
	return 0, errors.New("accepted run not found in thread")
}

func (e *Engine) readBoundFile(ctx context.Context, w config.Workflow, env conversation.Envelope, file attachments.InputFile, sha string) ([]byte, error) {
	check := func() error {
		audience, _, err := e.confirmAudience(ctx, w, env.Key())
		if err != nil {
			return errors.New("current attachment audience unavailable")
		}
		if !slices.Equal(audience, env.AccessProof.AudienceSubjects) {
			return errors.New("accepted attachment audience changed")
		}
		post, err := e.MM.Post(ctx, env.Anchor)
		if err != nil {
			return inputHTTPError("post", err)
		}
		if post.ID != env.Anchor || post.Root() != env.Root || post.ChannelID != env.Channel ||
			post.DeleteAt != 0 || post.UserID != env.AccessContext.Subject.ID || !slices.Contains(env.Versions, conversation.PostVersion(post)) || !slices.Contains(post.FileIDs, file.FileID) {
			return errors.New("accepted post identity or version changed")
		}
		return e.workflowGate(ctx, w, env, audience, []string{post.UserID})
	}
	if err := check(); err != nil {
		return nil, err
	}
	infos, err := e.MM.Files(ctx, file.PostID)
	if err != nil {
		return nil, inputHTTPError("post file list", err)
	}
	found := 0
	for _, info := range infos {
		if info.ID == file.FileID {
			if info.PostID != file.PostID || info.Size != file.Size || info.MIME != file.MIME || info.Name != file.Name {
				return nil, errors.New("accepted attachment metadata changed")
			}
			found++
		}
	}
	maxBytes := min(w.Files.MaxFileBytes, env.Request.Limits.MaxFileBytes)
	if strings.HasPrefix(file.MIME, "image/") {
		maxBytes = min(w.Files.MaxImageBytes, env.Request.Limits.MaxImageBytes)
	}
	maxBytes = min(maxBytes, w.Files.MaxBatchBytes, env.Request.Limits.MaxBatchBytes)
	if found != 1 || file.Size < 0 || file.Size > maxBytes {
		return nil, errors.New("accepted file absent or outside limits")
	}
	source, ok := e.MM.(interface {
		Download(context.Context, string, int64) ([]byte, error)
	})
	if !ok {
		return nil, errors.New("attachment download unsupported")
	}
	data, err := source.Download(ctx, file.FileID, file.Size)
	if err != nil {
		return nil, inputHTTPError("file download", err)
	}
	if int64(len(data)) != file.Size || attachments.Hash(data) != sha {
		return nil, errors.New("accepted file size or digest mismatch")
	}
	if err := check(); err != nil {
		return nil, err
	}
	return data, nil
}

func inputHTTPError(stage string, err error) error {
	if status := mattermost.Status(err); status != 0 {
		return fmt.Errorf("%s: HTTP %d", stage, status)
	}
	return fmt.Errorf("%s: transport unavailable", stage)
}
