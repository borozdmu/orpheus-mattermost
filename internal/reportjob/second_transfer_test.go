package reportjob

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestPublicationSecondMethodFencesAllEarlierWriters(t *testing.T) {
	for _, order := range []string{"replacement-first", "result-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			path, approval := journalFixture(t)
			approval.Run, approval.Stage = uuid.NewString(), 10
			record := func(v PublicationEvent) error {
				j := testJournal(t, path, approval)
				return j.Record(t.Context(), v)
			}
			require := func(v PublicationEvent) {
				t.Helper()
				if err := record(v); err != nil {
					t.Fatal(v.Action, err)
				}
			}
			denied := func(v PublicationEvent) {
				t.Helper()
				if record(v) == nil {
					t.Fatal("unexpected admission", v.Action)
				}
			}
			one := PublicationEvent{Stage: approval.Stage, Run: approval.Run, Invocation: uuid.NewString(), Action: "claim", Digest: approval.Digest()}
			require(one)
			one.Action, one.Run, one.Digest = "open", uuid.NewString(), strings.Repeat("1", 64)
			require(one)
			two := one
			two.Action, two.Invocation = "handoff", uuid.NewString()
			require(two)
			transfer := two
			transfer.Action, transfer.Invocation = "method-change", uuid.NewString()
			denied(transfer)
			two.Action, two.Run, two.Digest = "open", uuid.NewString(), strings.Repeat("2", 64)
			require(two)
			transfer.Run, transfer.Digest = two.Run, two.Digest
			bad := transfer
			bad.Invocation = one.Invocation
			denied(bad)
			approval.Audit = strings.Repeat("f", 64)
			denied(transfer)
			approval.Audit = ""
			late := two
			late.Action, late.Digest = "result", strings.Repeat("b", 64)
			var changed, published error
			switch order {
			case "replacement-first":
				changed, published = record(transfer), record(late)
			case "result-first":
				published, changed = record(late), record(transfer)
			default:
				var group sync.WaitGroup
				group.Go(func() { changed = record(transfer) })
				group.Go(func() { published = record(late) })
				group.Wait()
			}
			if (changed == nil) == (published == nil) {
				t.Fatal("expected one writer", changed, published)
			}
			denied(transfer)
			if changed != nil {
				return
			}
			denied(one)
			denied(two)
			denied(late)
			newMethod := transfer
			newMethod.Run, newMethod.Action, newMethod.Digest = uuid.NewString(), "open", strings.Repeat("3", 64)
			require(newMethod)
			another := newMethod
			another.Invocation, another.Action = uuid.NewString(), "method-change"
			denied(another)
			newMethod.Action, newMethod.Digest = "result", strings.Repeat("4", 64)
			require(newMethod)
		})
	}
}
