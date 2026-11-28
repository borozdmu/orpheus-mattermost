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
			one := PublicationEvent{Stage: 10, Run: uuid.NewString(), Invocation: uuid.NewString(), Action: "open", Digest: strings.Repeat("a", 64)}
			require(one)
			one.Digest = strings.Repeat("1", 64)
			for _, action := range []string{"result", "upload", "publish"} {
				one.Action = action
				require(one)
			}
			two := PublicationEvent{Stage: 11, Run: uuid.NewString(), Invocation: uuid.NewString(), Action: "open", Digest: strings.Repeat("2", 64)}
			transfer := PublicationEvent{Stage: 11, Run: two.Run, Invocation: uuid.NewString(), Action: "method-change", Digest: two.Digest,
				Former: two.Invocation, PriorRun: one.Run, PriorSHA: one.Digest}
			denied(transfer)
			require(two)
			denied(transfer)
			one.Action = "receipt"
			require(one)
			bad := transfer
			bad.Invocation = transfer.Former
			denied(bad)
			bad = transfer
			bad.PriorSHA = strings.Repeat("9", 64)
			denied(bad)
			bad = transfer
			bad.PriorRun = uuid.NewString()
			denied(bad)
			bad = transfer
			bad.Former = uuid.NewString()
			denied(bad)
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
			denied(two)
			denied(late)
			newMethod := PublicationEvent{Stage: 11, Run: uuid.NewString(), Invocation: transfer.Invocation, Action: "open", Digest: strings.Repeat("3", 64)}
			require(newMethod)
			another := transfer
			another.Invocation, another.Former, another.Run, another.Digest = uuid.NewString(), newMethod.Invocation, newMethod.Run, newMethod.Digest
			denied(another)
			newMethod.Action, newMethod.Digest = "result", strings.Repeat("4", 64)
			require(newMethod)
		})
	}
}
