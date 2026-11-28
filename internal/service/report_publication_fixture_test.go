package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

// Exercise real file locking, persistence and parsing. The host-file metadata
// and append-only Linux checks have separate filesystem integration coverage.
func installTestPublication(t *testing.T, card reportjob.Card) {
	t.Helper()
	a := reportjob.RecoveryApproval{Job: card.Job, Card: card.Digest(), Journal: uuid.NewString(), Device: 1, Inode: 1}
	path := filepath.Join(t.TempDir(), "journal")
	if err := os.WriteFile(path, fmt.Appendf(nil, "orpheus.mattermost.publication.v1 %s %s %s\n", a.Journal, a.Job, a.Card), 0600); err != nil {
		t.Fatal(err)
	}
	old := openPublication
	openPublication = func(card reportjob.Card) (*reportjob.Publication, error) {
		if a.Card != card.Digest() {
			return nil, errReportBinding
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			return nil, err
		}
		return &reportjob.Publication{Approval: a, File: f}, nil
	}
	t.Cleanup(func() { openPublication = old })
}
