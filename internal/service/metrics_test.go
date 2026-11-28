package service

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestActiveRunGaugeIncludesUnfinishedAdmission(t *testing.T) {
	e, _, _, _, key := fixture(t)
	w := e.Config.Workflows[0]
	w.MaxConcurrentRuns = 2
	r := &Runtime{Engine: e, cfg: e.Config, initialized: true, active: map[conversation.Key]int{}}
	h := metricsHandler([]metricSource{{runtime: r}}, false)
	assertActive := func(want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		line := fmt.Sprintf("orpheus_mattermost_active_runs %d\n", want)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), line) {
			t.Fatalf("metrics response does not contain %q", line)
		}
	}
	assertActive(0)
	other := key
	other.Root = "other-thread"
	for i, k := range []conversation.Key{key, other} {
		if !r.reserve(k, w) {
			t.Fatal("could not reserve admission")
		}
		assertActive(i + 1)
	}
	var wg sync.WaitGroup
	r.observe(t.Context(), &wg, key, conversation.Snapshot{})
	assertActive(1)
	r.observe(t.Context(), &wg, other, conversation.Snapshot{})
	assertActive(0)
}

func TestSuspendedThreadMetrics(t *testing.T) {
	for _, withSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_labels=%v", withSource), func(t *testing.T) {
			e, _, _, _, key := fixture(t)
			r := &Runtime{Engine: e}
			sources := []metricSource{{runtime: r, source: "one"}}
			if withSource {
				sources = append(sources, metricSource{runtime: &Runtime{Engine: e}, source: "two"})
			}
			handler := metricsHandler(sources, withSource)
			assert := func(want int) {
				t.Helper()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
				body := rec.Body.String()
				label := ""
				if withSource {
					label = `{source="one"}`
					if !strings.Contains(body, "orpheus_mattermost_suspended_threads{source=\"two\"} 0\n") {
						t.Fatal("suspended thread counts leaked between sources", body)
					}
				}
				sample := fmt.Sprintf("orpheus_mattermost_suspended_threads%s %d\n", label, want)
				if rec.Code != 200 || !strings.Contains(body, "# TYPE orpheus_mattermost_suspended_threads gauge\n") || !strings.Contains(body, sample) {
					t.Fatal("incorrect suspended thread gauge", body)
				}
			}
			assert(0)
			second, transient := key, key
			second.Root, transient.Root = "second", "transient"
			r.mu.Lock()
			r.jobs = map[conversation.Key]*scheduled{
				key:       {Retry: retry{Permanent: true}},
				second:    {Retry: retry{Permanent: true}},
				transient: {Retry: retry{Failures: 1}},
			}
			r.mu.Unlock()
			assert(2)
			r.mu.Lock()
			r.jobs[key].Retry = retry{}
			r.mu.Unlock()
			assert(1)
			r.mu.Lock()
			delete(r.jobs, second)
			r.mu.Unlock()
			assert(0)
		})
	}
}
