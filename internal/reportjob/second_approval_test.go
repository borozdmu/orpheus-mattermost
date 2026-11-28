package reportjob

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secondApprovalFixture() (Card, MethodApproval, SecondApproval) {
	card, first := methodApprovalFixture()
	return card, first, SecondApproval{
		Schema: "orpheus.mattermost.method-approval.v2", Job: card.Job, Card: card.Digest(),
		Previous: first.Digest(), From: first.To, To: strings.Repeat("c", 64), FileName: "next.pyz",
		Checkpoint: strings.Repeat("d", 64), Position: 23, Stage: 10,
	}
}

func TestSecondApprovalImmutableChain(t *testing.T) {
	card, first, second := secondApprovalFixture()
	if !second.Valid(card, first) {
		t.Fatal("valid checkpoint method replacement refused")
	}
	for name, alter := range map[string]func(*SecondApproval){
		"old schema":     func(a *SecondApproval) { a.Schema = first.Schema },
		"other job":      func(a *SecondApproval) { a.Job = testCard().Job },
		"other sender":   func(a *SecondApproval) { c := card; c.Actor = strings.Repeat("s", 26); a.Card = c.Digest() },
		"other approval": func(a *SecondApproval) { c := first; c.FileName = "swapped.pyz"; a.Previous = c.Digest() },
		"first method":   func(a *SecondApproval) { a.From = first.From },
		"reuse current":  func(a *SecondApproval) { a.To = first.To },
		"rollback":       func(a *SecondApproval) { a.To = first.From },
		"invalid to":     func(a *SecondApproval) { a.To = strings.ToUpper(a.To) },
		"wrong filename": func(a *SecondApproval) { a.FileName = "../next.pyz" },
		"wrong type":     func(a *SecondApproval) { a.FileName = "next.zip" },
		"invalid digest": func(a *SecondApproval) { a.Checkpoint += "x" },
		"no progress":    func(a *SecondApproval) { a.Position = 0 },
		"no checkpoint":  func(a *SecondApproval) { a.Stage = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := second
			alter(&changed)
			if changed.Valid(card, first) || changed.Digest() == second.Digest() {
				t.Fatal("unapproved change accepted or retained the previous digest")
			}
		})
	}
	invalidFirst := first
	invalidFirst.Schema = second.Schema
	wrongChain := second
	wrongChain.Previous = invalidFirst.Digest()
	if wrongChain.Valid(card, invalidFirst) {
		t.Fatal("invalid first approval trusted because its hash matched")
	}
	for name, alter := range map[string]func(*SecondApproval){
		"stage":      func(a *SecondApproval) { a.Stage++ },
		"position":   func(a *SecondApproval) { a.Position++ },
		"checkpoint": func(a *SecondApproval) { a.Checkpoint = strings.Repeat("e", 64) },
		"filename":   func(a *SecondApproval) { a.FileName = "new.pyz" },
	} {
		t.Run("pin "+name, func(t *testing.T) {
			changed := second
			alter(&changed)
			if !changed.Valid(card, first) || changed.Digest() == second.Digest() {
				t.Fatal("different valid approval must have its own digest")
			}
		})
	}
	if got, want := second.Command("mark"), "@mark /report-upgrade-from-checkpoint "+second.Job+" "+first.To+" "+second.To+" 23 "+second.Checkpoint; got != want || got == first.Command("mark") {
		t.Fatal("second post must explicitly select both programs and the approved checkpoint")
	}
}

func TestSecondApprovalStrictJSONAndSource(t *testing.T) {
	card, first, second := secondApprovalFixture()
	raw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeSecondApproval(raw, card, first); err != nil || decoded != second {
		t.Fatalf("round-trip did not preserve the approved checkpoint: %v", err)
	}
	text := string(raw)
	for _, raw := range []string{
		strings.Replace(text, `"stage":`, `"extra":0,"stage":`, 1),
		strings.Replace(text, `"stage":`, `"stage":3,"stage":`, 1),
		strings.Replace(text, `"stage":`, `"STAGE":`, 1),
		strings.Replace(text, `"position":23,`, "", 1),
		strings.Replace(text, `"position":23`, `"position":"23"`, 1),
		strings.Replace(text, `"position":23`, `"position":23.5`, 1),
		text + " null", "null", "[]",
	} {
		if _, err := decodeSecondApproval([]byte(raw), card, first); err == nil {
			t.Fatal("malformed checkpoint approval accepted")
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "second.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MM_REPORT_METHOD_APPROVAL", file)
	t.Setenv("MM_REPORT_SECOND_METHOD_APPROVAL", "")
	if _, err := LoadSecondApproval(card, first); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("first approval path must never supply the second", err)
	}
	t.Setenv("MM_REPORT_SECOND_METHOD_APPROVAL", file)
	if os.Geteuid() != 0 {
		if _, err := LoadSecondApproval(card, first); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatal("untrusted operator file accepted or treated as absent")
		}
		return
	}
	if got, err := LoadSecondApproval(card, first); err != nil || got != second {
		t.Fatal("root-owned second approval must load", err)
	}
	t.Setenv("MM_REPORT_SECOND_METHOD_APPROVAL", filepath.Join(dir, "absent"))
	if _, err := LoadSecondApproval(card, first); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent file", err)
	}
	link := filepath.Join(dir, "indirect.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MM_REPORT_SECOND_METHOD_APPROVAL", link)
	if _, err := LoadSecondApproval(card, first); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlinked approval accepted or treated as absent")
	}
}
