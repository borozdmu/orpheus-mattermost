package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

func TestScheduleWitnessSealsCurrentChannelWithoutPosting(t *testing.T) {
	corporateDirectory(t)
	e, core, mm, _, key, w := protectedFixture(t)
	e.SourceID = key.Source
	w.AgentID = "mark"
	w.Access.VerifyAccountCards = true
	w.Access.ScopeContext = map[string]any{"agent_id": "mark", "policy_space_id": "mark-test"}
	w.Access.IdentityProjection.ActorField = "actor_user_id"
	w.Access.IdentityProjection.AudienceField = "reader_user_ids"
	root := corporateStateBinding(w, key.Channel)
	root.Services = []string{"kord"}
	e.Config.Workflows[0] = root
	resolved, err := root.ForChat(key.Channel)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("[42]"))
	now := time.Now().UTC().Truncate(30 * time.Minute).Add(time.Minute)
	p := registeredOccurrence{Schedule: uuid.New(), Occurrence: uuid.New(), Slot: now.Add(-time.Minute), Source: e.SourceID, Agent: "mark", Actor: mm.posts[0].UserID, Principal: "42", ReadersDigest: hex.EncodeToString(digest[:]), Revision: resolved.EffectiveRevision, Text: "Scheduled access check", Chat: key.Channel}

	out, err := os.CreateTemp(t.TempDir(), "events")
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = out
	a, err := e.scheduleWitness(t.Context(), p, now)
	os.Stdout = prev
	if err != nil {
		t.Fatal(err)
	}
	task, err := agentcontract.Decode(*a.Assignment)
	if err != nil || task.InvocationID != p.Occurrence || task.Delivery.Connector != "mattermost" || task.Delivery.Chat != key.Channel ||
		task.Delivery.Thread != "space-schedule:"+p.Schedule.String() || task.Delivery.Message != "space-occurrence:"+p.Occurrence.String() ||
		task.Author.PrincipalID != "42" || task.Text != p.Text || task.Verify(strings.Repeat("a", 32), time.Now()) != nil {
		t.Fatal("wrong scheduled assignment")
	}
	if len(core.attempts) != 0 {
		t.Fatal("witness must not dispatch")
	}
	raw, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]int{}
	for line := range strings.Lines(string(raw)) {
		var event map[string]string
		if json.Unmarshal([]byte(line), &event) != nil || event["kind"] != "kord.route.v1" {
			continue
		}
		if event["trace"] != occurrenceTrace(p.Occurrence) {
			t.Fatal("event without the occurrence trace")
		}
		stages[event["stage"]+"/"+event["decision"]]++
	}
	if stages["rpc/"] == 0 || stages["rpc/"] != stages["decision/allow"] {
		t.Fatalf("missing route events %v", stages)
	}

	if _, err = e.scheduleWitness(t.Context(), p, p.Slot.Add(5*time.Minute)); err == nil {
		t.Fatal("stale occurrence")
	}
	offGrid := p
	offGrid.Slot = p.Slot.Add(time.Minute)
	if _, err = e.scheduleWitness(t.Context(), offGrid, offGrid.Slot); err == nil {
		t.Fatal("unaligned occurrence")
	}
	other := p
	other.Principal = "43"
	if _, err = e.scheduleWitness(t.Context(), other, now); err == nil {
		t.Fatal("another principal")
	}
	other = p
	other.Revision = "old"
	if _, err = e.scheduleWitness(t.Context(), other, now); err == nil {
		t.Fatal("old revision")
	}
	mm.audience = append(mm.audience, "nnnnnnnnnnnnnnnnnnnnnnnnnn")
	if _, err = e.scheduleWitness(t.Context(), p, now); err == nil {
		t.Fatal("changed roster")
	}
}

func TestScheduledAssignmentRequiresCurrentRegistration(t *testing.T) {
	scheduleRegistryDir = t.TempDir()
	t.Cleanup(func() { scheduleRegistryDir = "/run/approved-schedule-occurrences" })
	o := uuid.New()
	a := agentcontract.Assignment{InvocationID: o, Delivery: agentcontract.Route{Thread: "space-schedule:" + uuid.NewString(), Message: "space-occurrence:" + o.String()}}
	// The temporary directory is not root-owned: an unverified registry never confirms.
	if currentSchedule(a, "source", time.Now()) {
		t.Fatal("unverified registration confirmed")
	}
	a.Delivery.Thread = "inbox"
	if currentSchedule(a, "source", time.Now()) {
		t.Fatal("mixed route confirmed")
	}
	if !currentSchedule(agentcontract.Assignment{Delivery: agentcontract.Route{Thread: "root", Message: "post"}}, "source", time.Now()) {
		t.Fatal("ordinary Mattermost route denied")
	}
}

func TestScheduleTraceIsTheObserverOccurrenceTrace(t *testing.T) {
	o := uuid.MustParse("6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab")
	// Python: uuid.uuid5(occurrence, "audience/confirm").hex
	if occurrenceTrace(o) != "64c078ccd00259e9b42b619e9cbdf6f6" {
		t.Fatal("observer cannot derive the same trace")
	}
	s := "space-schedule:" + uuid.NewString()
	got, _ := scheduleTrace(t.Context(), s, "space-occurrence:"+o.String()).Value(directoryProbeKey{}).(string)
	if got != occurrenceTrace(o) {
		t.Fatal("scheduled assignment has no trace")
	}
	for _, tc := range [][2]string{{"inbox", "space-occurrence:" + o.String()}, {s, "42"}, {s, "space-occurrence:" + strings.ToUpper(o.String())}, {s, "space-occurrence:" + uuid.Nil.String()}} {
		if v, _ := scheduleTrace(t.Context(), tc[0], tc[1]).Value(directoryProbeKey{}).(string); v != "" {
			t.Fatal("ordinary or malformed route traced")
		}
	}
	explicit := context.WithValue(t.Context(), directoryProbeKey{}, strings.Repeat("a", 32))
	if v, _ := scheduleTrace(explicit, s, "space-occurrence:"+o.String()).Value(directoryProbeKey{}).(string); v != strings.Repeat("a", 32) {
		t.Fatal("explicit probe replaced")
	}
}

func TestOccurrenceRequestDoesNotAcceptIdentityOverrides(t *testing.T) {
	key := strings.Repeat("k", 42)
	s, o := uuid.NewString(), uuid.NewString()
	reg := occurrenceRegistry{path: t.TempDir(), token: key}
	for _, tc := range []struct {
		key, body, query string
		multiple         bool
	}{
		{"wrong", "", "", false}, {key, "actor=1", "", false}, {key, "", "?owner=1", false}, {key, "", "", true}, {key, "", "", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/internal/schedules/"+s+"/occurrences/"+o+tc.query, strings.NewReader(tc.body))
		r.SetPathValue("schedule", s)
		r.SetPathValue("occurrence", o)
		r.Header.Set("Authorization", "Bearer "+tc.key)
		if tc.multiple {
			r.Header.Add("Authorization", "Bearer "+key)
		}
		if _, err := reg.approved(r); err == nil {
			t.Fatal("untrusted registration accepted")
		}
	}
}
