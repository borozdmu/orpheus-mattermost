package service

import (
	"context"
	"errors"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type acceptedFileSource struct {
	*accessSource
	downloads   int
	after       func()
	downloadErr error
}

func (s *acceptedFileSource) Me(context.Context) (mattermost.User, error) {
	return mattermost.User{ID: s.bot, Username: "fixture"}, nil
}

func (s *acceptedFileSource) Post(_ context.Context, id string) (mattermost.Post, error) {
	for _, p := range s.posts {
		if p.ID == id {
			return p, nil
		}
	}
	return mattermost.Post{}, &mattermost.HTTPError{Status: 404}
}

func (s *acceptedFileSource) Download(_ context.Context, _ string, limit int64) ([]byte, error) {
	if limit != 3 {
		return nil, errors.New("unexpected size")
	}
	s.downloads++
	if s.after != nil {
		s.after()
	}
	return []byte("png"), s.downloadErr
}

func TestAcceptedFileProbeUsesImmutableTriggerAndCurrentAudience(t *testing.T) {
	const file = "ffffffffffffffffffffffffff"
	for _, tc := range []struct {
		kind      string
		downloads int
	}{
		{"valid", 1}, {"wrong_hash", 1}, {"http_forbidden", 1}, {"other_run", 0}, {"other_file", 0},
		{"edited", 0}, {"new_author", 0}, {"post_moved", 0}, {"file_removed", 0}, {"audience_changed", 0},
		{"audience_changed_after_read", 1}, {"post_changed_after_read", 1}, {"undelivered", 0},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e, a, mm, _, key, w := protectedFixture(t)
			source := &acceptedFileSource{accessSource: mm}
			e.MM = source
			key.Source = w.Mattermost.Source(mm.bot)
			mm.posts[0].FileIDs = []string{file}
			if err := e.Thread(t.Context(), w, key, mm.channel, true); err != nil {
				t.Fatal(err)
			}
			s := &a.snapshot.Sessions[0]
			s.ExternalKey = key.External()
			runID, fileID, digest := s.Runs[0].ID, file, attachments.Hash([]byte("png"))
			changeAudience := func() { mm.audience = append(mm.audience, "nnnnnnnnnnnnnnnnnnnnnnnnnn") }
			changePost := func() { mm.posts[0].Message += " modified" }
			switch tc.kind {
			case "wrong_hash":
				digest = attachments.Hash([]byte("other"))
			case "other_run":
				runID = "00000000-0000-4000-8000-000000000099"
			case "other_file":
				fileID = "eeeeeeeeeeeeeeeeeeeeeeeeee"
			case "http_forbidden":
				source.downloadErr = &mattermost.HTTPError{Status: 403}
			case "edited":
				changePost()
			case "new_author":
				mm.posts[0].UserID = mm.bot
			case "post_moved":
				mm.posts[0].RootID = file
			case "file_removed":
				mm.posts[0].FileIDs = nil
			case "audience_changed":
				changeAudience()
			case "audience_changed_after_read":
				source.after = changeAudience
			case "post_changed_after_read":
				source.after = changePost
			case "undelivered":
				s.Messages[len(s.Messages)-1].Delivery = "rejected"
			}
			size, err := e.CheckAcceptedFile(t.Context(), w, key, runID, key.Root, fileID, digest)
			if (err == nil) != (tc.kind == "valid") || source.downloads != tc.downloads || (size == 3) != (err == nil) {
				t.Fatalf("size %d downloads %d error %v", size, source.downloads, err)
			}
			if tc.kind == "http_forbidden" && err.Error() != "file download: HTTP 403" {
				t.Fatal(err)
			}
		})
	}
}
