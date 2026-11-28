package reportjob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testCard() Card {
	return Card{Schema: "orpheus.mattermost.report-card.v1", Job: uuid.NewString(), Workflow: "test", Channel: strings.Repeat("c", 26), Thread: strings.Repeat("t", 26), Actor: strings.Repeat("a", 26), Method: strings.Repeat("e", 64), Source: "my", Revision: "test-v1", URL: "https://beta-kord.skillum.ai" + BasePath}
}

func TestMethodDiagnosticInvocation(t *testing.T) {
	anchor := uuid.New()
	s := Stage{Job: uuid.NewString(), Number: 3, Attempt: 1, RetryRun: uuid.NewString(), Method: &MethodRevision{First: 2}}
	previousForm, err := json.Marshal([]any{"orpheus.mattermost.method-invocation.v1", s.Job, s.Number, s.Method, s.Prior, s.Attempt, s.RetryRun})
	if err != nil {
		t.Fatal(err)
	}
	ordinary := s.Invocation(anchor)
	if ordinary != uuid.NewSHA1(anchor, previousForm) {
		t.Fatal("existing ordinary method invocation changed")
	}
	s.Diagnostic = true
	diagnostic := s.Invocation(anchor)
	if diagnostic == ordinary || diagnostic != s.Invocation(anchor) {
		t.Fatal("diagnostic must have a distinct, repeatable identity")
	}
}

func TestCardPinAndScope(t *testing.T) {
	c := testCard()
	if !c.Valid() {
		t.Fatal("valid card rejected")
	}
	for _, raw := range []string{"http://beta-kord.skillum.ai" + BasePath, "https://beta-kord.skillum.ai.other" + BasePath, "https://user@beta-kord.skillum.ai" + BasePath, "https://beta-kord.skillum.ai" + BasePath + "extra"} {
		v := c
		v.URL = raw
		if v.Valid() {
			t.Fatal("unexpected URL accepted")
		}
	}
	now := time.Now().Truncate(time.Second)
	s := Stage{Card: c.Digest(), Job: c.Job, Number: 1}
	token := s.Token("unit", uuid.NewString(), strings.Repeat("d", 64), now)
	if !s.Valid(c) || !ValidToken(token, "unit", c.Job, now) {
		t.Fatal("stage rejected")
	}
	if ValidToken(token, "wrong", c.Job, now) || ValidToken(token, "unit", uuid.NewString(), now) || ValidToken(token, "unit", c.Job, now.Add(20*time.Minute)) || ValidToken(token, "unit", c.Job, now.Add(-time.Second)) {
		t.Fatal("scope or time missing")
	}
	edited := c
	edited.Actor = strings.Repeat("z", 26)
	if s.Valid(edited) {
		t.Fatal("card replaced")
	}
}

func TestCardRejectsUntrustedInput(t *testing.T) {
	card := testCard()
	raw, _ := json.Marshal(card)
	if exactJSONFields([]byte(`{"name": 12, "name": 13}`), "name") || exactJSONFields([]byte(`{"name":12,"x":13}`), "name") || exactJSONFields([]byte(`{"name":12}{}`), "name") {
		t.Fatal("accepted ambiguous card")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "card.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MM_REPORT_JOB_CARD", file)
	if os.Geteuid() == 0 {
		if c, err := Load(); err != nil || c != card {
			t.Fatal("root-owned card", err)
		}
	} else if _, err := Load(); err == nil {
		t.Fatal("non-root card accepted")
	}
	if err := os.Chmod(file, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("writable card accepted")
	}
	link := filepath.Join(dir, "other")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MM_REPORT_JOB_CARD", link)
	if _, err := Load(); err == nil {
		t.Fatal("symlink accepted")
	}
}
