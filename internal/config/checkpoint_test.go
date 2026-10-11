package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func checkpointWorkflow(t *testing.T) Workflow {
	t.Helper()
	cfg, err := example(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Workflows[0]
	w.IncludeIDs = []string{strings.Repeat("c", 26)}
	w.Links.Enabled = new(false)
	w.Since = time.UnixMilli(200)
	w.AdmissionCheckpoint = &AdmissionCheckpoint{Schema: "mattermost-admission-checkpoint.v1", ChannelID: w.IncludeIDs[0], BotID: strings.Repeat("b", 26), Epoch: uuid.NewString(), FloorMS: 100, ReplayFromMS: 200, ConsumedPostIDs: []string{strings.Repeat("p", 26)}}
	return w
}

func TestCheckpointRejectsIncompleteOrMismatchedConfiguration(t *testing.T) {
	for name, change := range map[string]func(*Workflow){
		"schema":         func(w *Workflow) { w.AdmissionCheckpoint.Schema = "v2" },
		"nil epoch":      func(w *Workflow) { w.AdmissionCheckpoint.Epoch = uuid.Nil.String() },
		"epoch":          func(w *Workflow) { w.AdmissionCheckpoint.Epoch = "invalid" },
		"channel":        func(w *Workflow) { w.AdmissionCheckpoint.ChannelID = "wrong" },
		"bot":            func(w *Workflow) { w.AdmissionCheckpoint.BotID = "wrong" },
		"negative floor": func(w *Workflow) { w.AdmissionCheckpoint.FloorMS = -1 },
		"reverse time":   func(w *Workflow) { w.AdmissionCheckpoint.ReplayFromMS = 99 },
		"later replay":   func(w *Workflow) { w.Since = time.UnixMilli(201) },
		"submillisecond": func(w *Workflow) { w.Since = time.UnixMilli(200).Add(time.Nanosecond) },
		"all channels":   func(w *Workflow) { w.IncludeIDs = nil },
		"links":          func(w *Workflow) { w.Links.Enabled = new(true) },
		"DM":             func(w *Workflow) { w.DirectMessages = true },
		"duplicates": func(w *Workflow) {
			w.AdmissionCheckpoint.ConsumedPostIDs = []string{strings.Repeat("p", 26), strings.Repeat("p", 26)}
		},
		"unsorted": func(w *Workflow) {
			w.AdmissionCheckpoint.ConsumedPostIDs = []string{strings.Repeat("z", 26), strings.Repeat("p", 26)}
		},
		"ID": func(w *Workflow) { w.AdmissionCheckpoint.ConsumedPostIDs = []string{"wrong"} },
	} {
		t.Run(name, func(t *testing.T) {
			w := checkpointWorkflow(t)
			change(&w)
			if w.validateCheckpoint() == nil {
				t.Fatal("unsafe checkpoint accepted")
			}
		})
	}
}

func TestCheckpointSameMillisecondHasNoLexicalWatermark(t *testing.T) {
	w := checkpointWorkflow(t)
	if err := w.validateCheckpoint(); err != nil {
		t.Fatal(err)
	}
	c := w.AdmissionCheckpoint
	if c.Allows(strings.Repeat("p", 26), 200) || c.Allows(strings.Repeat("p", 26), 201) || c.Allows(strings.Repeat("a", 26), 199) || !c.Allows(strings.Repeat("a", 26), 200) || !c.Allows(strings.Repeat("z", 26), 201) {
		t.Fatal("checkpoint lost or replayed a post")
	}
}

func TestCheckpointEpochChangesSessionRevisionButDrainingDoesNot(t *testing.T) {
	base := workflowText(t)
	addition := "admission_checkpoint:\n  schema: mattermost-admission-checkpoint.v1\n  channel_id: cccccccccccccccccccccccccc\n  bot_id: bbbbbbbbbbbbbbbbbbbbbbbbbb\n  epoch: 11111111-1111-4111-8111-111111111111\n  floor_ms: 100\n  replay_from_ms: 200\n  consumed_post_ids: [pppppppppppppppppppppppppp]\n"
	// Reuse the example defaults, overriding conflicting fields through one parsed config.
	var revisions []string
	for _, epoch := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
		dir := t.TempDir()
		writeWorkflow(t, dir, "assistant.md", strings.Replace(base, "---\n", "---\n"+strings.ReplaceAll(addition, "11111111-1111-4111-8111-111111111111", epoch), 1))
		cfg, err := example(t, map[string]string{"WORKFLOWS_DIR": dir})
		// The example routes all channels and its replay time differs, so an
		// uncoordinated checkpoint insertion must fail before serving.
		if err == nil || len(cfg.Workflows) != 0 {
			t.Fatal("uncoordinated workflow accepted")
		}
		cfg, err = example(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		w := checkpointWorkflow(t)
		w.AdmissionCheckpoint.Epoch = epoch
		w.ReconcileFrom = time.UnixMilli(200).UTC().Format(time.RFC3339Nano)
		cfg.Workflows = []Workflow{w}
		if err = cfg.validate(); err != nil {
			t.Fatal(err)
		}
		revision := cfg.Workflows[0].EffectiveRevision
		cfg.Workflows[0].Draining = true
		if err = cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Workflows[0].EffectiveRevision != revision {
			t.Fatal("draining changed session revision")
		}
		revisions = append(revisions, revision)
	}
	if revisions[0] == revisions[1] {
		t.Fatal("ownership periods reused session revision")
	}
}

// Produced by CXassist's real exporter on a drained synthetic SQLite queue.
func TestPrototypeExportFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/channel-checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	var cp AdmissionCheckpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	w := checkpointWorkflow(t)
	w.AdmissionCheckpoint = &cp
	if err = w.validateCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if !cp.Allows(strings.Repeat("a", 26), 200) || cp.Allows(strings.Repeat("p", 26), 200) {
		t.Fatal("prototype boundary was not preserved")
	}
}

func TestCheckpointRoundTripsThroughWorkflowLoader(t *testing.T) {
	w := checkpointWorkflow(t)
	parts := strings.SplitN(workflowText(t), "---", 3)
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &fields); err != nil {
		t.Fatal(err)
	}
	fields["admission_checkpoint"] = w.AdmissionCheckpoint
	fields["include_ids"] = w.IncludeIDs
	fields["link_expansion"] = map[string]bool{"enabled": false}
	fields["reconcile_from"] = time.UnixMilli(200).UTC().Format(time.RFC3339Nano)
	fields["draining"] = true
	raw, err := yaml.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeWorkflow(t, dir, "checkpoint.md", "---\n"+string(raw)+"---\n"+parts[2])
	cfg, err := example(t, map[string]string{"WORKFLOWS_DIR": dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workflows[0].AdmissionCheckpoint.Epoch != w.AdmissionCheckpoint.Epoch {
		t.Fatal("loader lost checkpoint")
	}
}
