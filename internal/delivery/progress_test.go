package delivery

import (
	"context"
	"encoding/json"
	"testing"
)

type progressMM struct {
	*fakeMM
	deleted []string
}

func (f *progressMM) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	for i := range f.posts {
		if f.posts[i].ID == id {
			f.posts[i].DeleteAt = 1
		}
	}
	return nil
}

func TestOnlyOwnUnchangedProgressIsDeleted(t *testing.T) {
	p, base, part := setup()
	mm := &progressMM{fakeMM: base}
	p.MM = mm
	part.Receipt.Attachments = nil
	part.Receipt.Temporary = true
	if err := p.Publish(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	original := mm.posts[0]
	for i, kind := range []string{"user", "other-run", "edited", "file"} {
		post := original
		post.ID = kind
		var r Receipt
		if err := json.Unmarshal(post.Props[Property], &r); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			post.UserID = "human"
		case 1:
			r.RunID = "other"
		case 2:
			post.Message = "changed by user"
		case 3:
			post.FileIDs = []string{"file"}
		}
		raw, _ := json.Marshal(r)
		post.Props = map[string]json.RawMessage{Property: raw}
		mm.posts = append(mm.posts, post)
	}
	if err := p.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupProgress(t.Context(), "session", "run"); err != nil {
		t.Fatal(err)
	}
	if len(mm.deleted) != 1 || mm.deleted[0] != original.ID {
		t.Fatal(mm.deleted)
	}
	// A restart sees the tombstone and cannot delete another post.
	if err := p.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupProgress(t.Context(), "session", "run"); err != nil {
		t.Fatal(err)
	}
	if len(mm.deleted) != 1 {
		t.Fatal(mm.deleted)
	}
}

func TestCostBasisSurvivesReconciliation(t *testing.T) {
	p, _, part := setup()
	part.Receipt.CostFooter = "frozen estimate"
	part.Receipt.Attachments = nil
	if err := p.Publish(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	if got := p.FrozenCost("session", "run"); got != "frozen estimate" {
		t.Fatal(got)
	}
	if got := p.FrozenCost("session", "other"); got != "" {
		t.Fatal(got)
	}
}
