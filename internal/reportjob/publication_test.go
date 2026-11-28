package reportjob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func journalFixture(t *testing.T) (string, RecoveryApproval) {
	t.Helper()
	c := testCard()
	a := RecoveryApproval{Job: c.Job, Card: c.Digest(), Journal: uuid.NewString(), Device: 1, Inode: 2}
	path := filepath.Join(t.TempDir(), "journal")
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s%s %s %s\n", journalHeader, a.Journal, a.Job, a.Card), 0600); err != nil {
		t.Fatal(err)
	}
	return path, a
}

func testJournal(t *testing.T, path string, a RecoveryApproval) Publication {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return Publication{Approval: a, File: f}
}

func TestPublicationKeepsOneLogicalResultAcrossReopens(t *testing.T) {
	path, a := journalFixture(t)
	v := PublicationEvent{Stage: 10, Run: uuid.NewString(), Invocation: uuid.NewString(), Action: "open", Digest: strings.Repeat("a", 64)}
	write := func(v PublicationEvent) error { j := testJournal(t, path, a); return j.Record(t.Context(), v) }
	if err := write(v); err != nil {
		t.Fatal(err)
	}
	other := v
	other.Run = uuid.NewString()
	other.Invocation = uuid.NewString()
	if err := write(other); err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			j := testJournal(t, path, a)
			entry := v
			entry.Action = "result"
			if j.Record(t.Context(), entry) == nil {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("found %d claims for the same result", won.Load())
	}
	j := testJournal(t, path, a)
	if j.CheckUnsubmitted(t.Context(), v) == nil {
		t.Fatal("result already pending")
	}
	for _, action := range []string{"result", "upload", "publish"} {
		other.Action = action
		if write(other) == nil {
			t.Fatal("foreign stage result allowed", action)
		}
	}
	for _, action := range []string{"upload", "publish", "receipt", "receipt"} {
		v.Action = action
		if err := write(v); err != nil {
			t.Fatal(action, err)
		}
	}
	old := v
	old.Action = "publish"
	if write(old) == nil {
		t.Fatal("second publish")
	}
	v.Stage++
	v.Run = uuid.NewString()
	v.Invocation = uuid.NewString()
	v.Action = "open"
	if err := write(v); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRejectsDamagedOrReplacedJournal(t *testing.T) {
	for _, damage := range []string{"empty", "partial", "duplicate field", "bad action", "changed identity"} {
		t.Run(damage, func(t *testing.T) {
			path, a := journalFixture(t)
			j := testJournal(t, path, a)
			switch damage {
			case "empty":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "partial":
				if _, err := j.File.WriteString("{"); err != nil {
					t.Fatal(err)
				}
			case "duplicate field":
				if _, err := j.File.WriteString("{\"stage\":1,\"stage\":2}\n"); err != nil {
					t.Fatal(err)
				}
			case "bad action":
				if _, err := fmt.Fprintf(j.File, "{\"stage\":1,\"run\":%q,\"invocation\":%q,\"action\":\"publish\",\"digest\":%q}\n", uuid.NewString(), uuid.NewString(), strings.Repeat("d", 64)); err != nil {
					t.Fatal(err)
				}
			case "changed identity":
				j.Approval.Journal = uuid.NewString()
			}
			v := PublicationEvent{Stage: 1, Run: uuid.NewString(), Invocation: uuid.NewString(), Action: "open", Digest: strings.Repeat("b", 64)}
			if j.Record(t.Context(), v) == nil {
				t.Fatal("accepted damaged ledger")
			}
		})
	}
}

func TestPublicationClaimAndLockDeadline(t *testing.T) {
	path, a := journalFixture(t)
	j := testJournal(t, path, a)
	locked := testJournal(t, path, a)
	if err := syscall.Flock(int(locked.File.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	v := PublicationEvent{Stage: 2, Run: uuid.NewString(), Invocation: uuid.NewString(), Action: "claim", Digest: strings.Repeat("c", 64)}
	if err := j.Record(ctx, v); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("did not stop for cancelled lock", err)
	}
	if err := syscall.Flock(int(locked.File.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if j.Record(t.Context(), v) == nil {
		t.Fatal("operator approval reused")
	}
	v.Action = "open"
	v.Run = uuid.NewString()
	foreign := v
	foreign.Invocation = uuid.NewString()
	if j.Record(t.Context(), foreign) == nil {
		t.Fatal("another invocation used claimed stage")
	}
	if err := j.Record(t.Context(), v); err != nil {
		t.Fatal(err)
	}
}
