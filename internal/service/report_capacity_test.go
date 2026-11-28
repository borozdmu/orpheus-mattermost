package service

import (
	"sync"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/reportjob"
)

func TestCheckpointedReportWaitsForCapacity(t *testing.T) {
	for _, blocker := range []string{"active run", "unobserved session"} {
		t.Run(blocker, func(t *testing.T) {
			f := reportExample(t)
			checkpoint := methodCheckpoint(t, f, 1, 11)
			publishMethodResult(t, f, reportjob.CheckpointName, checkpoint)
			f.w.MaxConcurrentRuns = 1

			r := observationRuntime(f.e, f.key)
			r.cfg = f.e.Config
			r.jobs[f.key] = &scheduled{Workflow: f.w}
			f.e.Reserve = r.reserve
			other := f.key
			other.Root = "nnnnnnnnnnnnnnnnnnnnnnnnnn"
			if blocker == "active run" {
				r.active[other] = 1
			} else {
				r.unknown[other] = true
			}
			var wg sync.WaitGroup
			work := func() {
				j := r.jobs[f.key]
				j.Working, j.Dirty, j.CapacityWait, r.running = true, false, false, 1
				r.work(t.Context(), t.Context(), &wg, f.key, f.w, f.mm.channel, true)
			}
			work()
			waiting := r.jobs[f.key]
			if waiting == nil || !waiting.CapacityWait || waiting.Dirty || len(f.api.attempts) != 1 {
				t.Fatal("checkpointed report was lost or admitted while capacity was unavailable")
			}
			r.observe(t.Context(), &wg, other, conversation.Snapshot{})
			if !waiting.Dirty {
				t.Fatal("capacity release did not wake the waiting report")
			}
			work()
			if len(f.api.attempts) != 2 {
				t.Fatalf("expected one continuation, got %d total admissions", len(f.api.attempts))
			}
			stage := f.api.attempts[1].Envelope.Report
			if stage == nil || stage.Number != 2 || stage.Prior == nil || stage.Prior.SHA != attachments.Hash(checkpoint) || stage.Prior.Position != 11 {
				t.Fatal("continuation did not use the published checkpoint")
			}
		})
	}
}
