package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type auditFiles struct {
	info mattermost.FileInfo
	err  error
}

func (f auditFiles) File(context.Context, string) (mattermost.FileInfo, error) { return f.info, f.err }

func TestAuditRequiresAttachedFileAndAllParts(t *testing.T) {
	key := conversation.Key{Source: "source", Workflow: "workflow", Channel: strings.Repeat("c", 26), Root: strings.Repeat("r", 26)}
	bot, pid, fid := strings.Repeat("b", 26), strings.Repeat("p", 26), strings.Repeat("f", 26)
	parts := Parts(key, "session", "run", "answer", "hello", conversation.Render{Version: 4, MaxChars: 64}, []attachments.Artifact{{ArtifactID: "artifact", FileID: fid, SHA256: "digest"}})
	raw, _ := json.Marshal(parts[0].Receipt)
	post := mattermost.Post{ID: pid, RootID: key.Root, ChannelID: key.Channel, UserID: bot, Message: "hello", FileIDs: []string{fid}, Props: map[string]json.RawMessage{Property: raw}}
	for _, tc := range []struct {
		name  string
		files auditFiles
		ok    bool
	}{
		{"valid", auditFiles{info: mattermost.FileInfo{ID: fid, PostID: pid}}, true},
		{"detached", auditFiles{info: mattermost.FileInfo{ID: fid}}, false},
		{"foreign post", auditFiles{info: mattermost.FileInfo{ID: fid, PostID: key.Root}}, false},
		{"unavailable", auditFiles{err: errors.New("read failed")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewAudit([]mattermost.Post{post}, key, bot)
			if err != nil {
				t.Fatal(err)
			}
			if a.Complete() == nil {
				t.Fatal("unverified receipt accepted")
			}
			err = a.Verify(t.Context(), tc.files, parts[0])
			if (err == nil) != tc.ok {
				t.Fatal(err)
			}
			if tc.ok && a.Complete() != nil {
				t.Fatal("verified receipt not complete")
			}
		})
	}
	post.FileIDs = nil
	a, err := NewAudit([]mattermost.Post{post}, key, bot)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verify(t.Context(), auditFiles{info: mattermost.FileInfo{ID: fid, PostID: pid}}, parts[0]) == nil {
		t.Fatal("missing attachment accepted")
	}
}
