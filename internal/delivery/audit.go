package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

// Audit verifies immutable receipts without a Create/Post/upload capability.
// Repairs and unknown receipt contracts fail closed for operator investigation.
type Audit struct {
	posts    []mattermost.Post
	receipts []Receipt
	used     []bool
}

func NewAudit(posts []mattermost.Post, key conversation.Key, bot string) (*Audit, error) {
	a := &Audit{}
	seen := map[string]bool{}
	for _, p := range posts {
		raw, has := p.Props[Property]
		if !has || p.UserID != bot {
			continue
		}
		var r Receipt
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, ErrConflict
		}
		if !errors.Is(dec.Decode(new(any)), io.EOF) {
			return nil, ErrConflict
		}
		if r.Source != key.Source || r.Workflow != key.Workflow {
			continue
		}
		if !config.ValidID(p.ID) || seen[p.ID] || p.ChannelID != key.Channel || p.Root() != key.Root || p.DeleteAt != 0 || p.EditAt != 0 || p.Type != "" || r.Schema != 1 || r.RenderVersion < 1 || r.RenderVersion > 4 || r.Parts < 1 || r.Parts > 10000 || r.Part < 0 || r.Part >= r.Parts || r.Hash != attachments.Hash([]byte(p.Message)) {
			return nil, ErrConflict
		}
		seen[p.ID] = true
		for _, old := range a.receipts {
			if sameKey(old, r) {
				return nil, ErrConflict
			}
		}
		a.posts = append(a.posts, p)
		a.receipts = append(a.receipts, r)
		a.used = append(a.used, false)
	}
	return a, nil
}

// Verify requires exactly the expected text/contract and every attached file.
func (a *Audit) Verify(ctx context.Context, files interface {
	File(context.Context, string) (mattermost.FileInfo, error)
}, part Part) error {
	for i, r := range a.receipts {
		if !sameKey(r, part.Receipt) {
			continue
		}
		// A file ID can change only through a separately verified repair. This first
		// exporter deliberately refuses repairs rather than guessing their outcome.
		expected := part.Receipt
		if len(r.Attachments) == 0 {
			r.Attachments = nil
		}
		if len(expected.Attachments) == 0 {
			expected.Attachments = nil
		}
		if !reflect.DeepEqual(r, expected) || a.posts[i].Message != part.Text {
			return ErrConflict
		}
		ids := []string{}
		for _, f := range r.Attachments {
			if !config.ValidID(f.FileID) || slices.Contains(ids, f.FileID) {
				return ErrConflict
			}
			info, err := files.File(ctx, f.FileID)
			if err != nil {
				return err
			}
			if info.ID != f.FileID || info.PostID != a.posts[i].ID {
				return ErrUpload
			}
			ids = append(ids, f.FileID)
		}
		actual := slices.Clone(a.posts[i].FileIDs)
		slices.Sort(actual)
		slices.Sort(ids)
		if !slices.Equal(actual, ids) {
			return ErrUpload
		}
		a.used[i] = true
		return nil
	}
	return errors.New("missing delivery receipt")
}

func (a *Audit) HasNotice(sid, rid, mid string) bool {
	for _, r := range a.receipts {
		if r.SessionID == sid && r.RunID == rid && r.MessageID == mid {
			return true
		}
	}
	return false
}

// Rejections also consume requests that never reached an Orpheus session.
func (a *Audit) Rejections() ([]string, error) {
	ids := map[string]bool{}
	for i, r := range a.receipts {
		if r.MessageID != "input_rejected" {
			continue
		}
		if r.SessionID != "" || r.RunID == "" || r.InputHash == "" || r.Revision == "" || len(r.TriggerIDs) == 0 || len(r.Attachments) != 0 || len(a.posts[i].FileIDs) != 0 {
			return nil, ErrConflict
		}
		versions := map[string]bool{}
		for _, v := range r.InputVersions {
			if !config.ValidID(v.ID) || versions[v.ID] || v.Hash == "" || !slices.Contains(r.TriggerIDs, v.ID) {
				return nil, ErrConflict
			}
			versions[v.ID] = true
		}
		for _, id := range r.TriggerIDs {
			if !config.ValidID(id) || !versions[id] {
				return nil, ErrConflict
			}
			ids[id] = true
		}
		count := 0
		for _, other := range a.receipts {
			if other.RunID != r.RunID || other.MessageID != r.MessageID {
				continue
			}
			x, y := r, other
			x.Attachments = nil
			y.Attachments = nil
			x.Part = 0
			y.Part = 0
			x.Hash = ""
			y.Hash = ""
			if !reflect.DeepEqual(x, y) {
				return nil, ErrConflict
			}
			count++
		}
		if count != r.Parts {
			return nil, errors.New("incomplete rejection receipt")
		}
		a.used[i] = true
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	slices.Sort(result)
	return result, nil
}

func (a *Audit) Complete() error {
	for _, used := range a.used {
		if !used {
			return errors.New("unrecognized or unverified delivery receipt")
		}
	}
	return nil
}
