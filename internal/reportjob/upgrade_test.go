package reportjob

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func methodApprovalFixture() (Card, MethodApproval) {
	c := testCard()
	return c, MethodApproval{
		Schema: "orpheus.mattermost.method-approval.v1", Job: c.Job, Card: c.Digest(),
		From: c.Method, To: strings.Repeat("b", 64), FileName: "mark-mql-adaptive.pyz",
	}
}

func TestMethodApprovalPinsOriginalCardAndBothMethods(t *testing.T) {
	card, a := methodApprovalFixture()
	if !a.Valid(card) {
		t.Fatal("valid method change rejected")
	}
	for _, field := range []string{"schema", "job", "card", "from", "to", "unchanged", "uppercase", "moved-channel", "different-actor", "invalid-original", "file-path", "windows-path", "file-extension", "empty-name"} {
		t.Run(field, func(t *testing.T) {
			edited, original := a, card
			switch field {
			case "schema":
				edited.Schema += ".v2"
			case "job":
				edited.Job = testCard().Job
			case "card":
				edited.Card = strings.Repeat("0", 64)
			case "from":
				edited.From = strings.Repeat("1", 64)
			case "to":
				edited.To = edited.To[:63]
			case "unchanged":
				edited.To = edited.From
			case "uppercase":
				edited.To = strings.ToUpper(edited.To)
			case "file-path":
				edited.FileName = "../mark.pyz"
			case "windows-path":
				edited.FileName = `..\mark.pyz`
			case "file-extension":
				edited.FileName = "mark-mql.zip"
			case "empty-name":
				edited.FileName = ".pyz"
			case "moved-channel":
				original.Channel = strings.Repeat("r", 26)
			case "different-actor":
				original.Actor = strings.Repeat("s", 26)
			case "invalid-original":
				original.URL = "https://example.test/"
				edited.Card = original.Digest()
			}
			if edited.Valid(original) {
				t.Fatal("mismatched approval accepted")
			}
			if edited != a && edited.Digest() == a.Digest() {
				t.Fatal("changed approval retained its digest")
			}
		})
	}
	renamed := a
	renamed.FileName = "other.pyz"
	if !renamed.Valid(card) || a.Digest() == renamed.Digest() {
		t.Fatal("approved file name missing from digest")
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMethodApproval(raw, card)
	if err != nil || decoded != a || decoded.Digest() != a.Digest() || !sha.MatchString(a.Digest()) {
		t.Fatal("approval did not round-trip", err)
	}
}

func TestMethodApprovalRequiresExactSchemaAndCommand(t *testing.T) {
	card, a := methodApprovalFixture()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	object := string(raw)
	for _, invalid := range []string{
		strings.Replace(object, `"to_sha256":`, `"to_sha256":"ignored","to_sha256":`, 1),
		strings.Replace(object, `"to_sha256":`, `"TO_SHA256":`, 1),
		strings.Replace(object, `"to_sha256":`, `"other":`, 1),
		strings.Replace(object, `"to_sha256":`, `"extra":null,"to_sha256":`, 1),
		object + object, object + " null", "null", "[]",
	} {
		if _, err := decodeMethodApproval([]byte(invalid), card); err == nil {
			t.Fatal("accepted malformed method approval")
		}
	}
	command := "@assistant /report-upgrade " + a.Job + " " + a.From + " " + a.To
	if a.Command("assistant") != command || !MatchesMethodCommand(" \n"+command+"\n", "assistant", a) {
		t.Fatal("genuine command rejected")
	}
	for _, text := range []string{
		strings.Replace(command, "@assistant", "@other", 1), strings.Replace(command, a.Job, card.Thread, 1),
		strings.Replace(command, a.From+" "+a.To, a.To+" "+a.From, 1), command[:len(command)-1],
		strings.Replace(command, a.From, a.From[:12], 1), command + " and continue", command + "\n" + command,
		card.Command("assistant"), "",
	} {
		if MatchesMethodCommand(text, "assistant", a) {
			t.Fatal("accepted different command", text)
		}
	}
}

func TestMethodApprovalProtectedFile(t *testing.T) {
	card, a := methodApprovalFixture()
	t.Setenv("MM_REPORT_METHOD_APPROVAL", "")
	if _, err := LoadMethodApproval(card); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unset operator path: %v", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "approval.json")
	t.Setenv("MM_REPORT_METHOD_APPROVAL", file)
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, err := LoadMethodApproval(card); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-root operator treated as trusted or absent: %v", err)
		}
		return
	}
	if got, err := LoadMethodApproval(card); err != nil || got != a {
		t.Fatal("protected root-owned approval", err)
	}
	t.Run("absent", func(t *testing.T) {
		t.Setenv("MM_REPORT_METHOD_APPROVAL", filepath.Join(dir, "absent"))
		if _, err := LoadMethodApproval(card); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	for _, issue := range []string{"file-mode", "file-owner", "dir-mode", "dir-owner", "file-symlink", "dangling-link", "ancestor-symlink", "hardlink", "fifo", "too-large", "relative", "dot-dot"} {
		t.Run(issue, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "operator")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "approval.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch issue {
			case "file-mode":
				err = os.Chmod(path, 0664)
			case "file-owner":
				err = os.Chown(path, 65534, -1)
			case "dir-mode":
				err = os.Chmod(parent, 0777)
			case "dir-owner":
				err = os.Chown(parent, 65534, -1)
			case "file-symlink":
				link := filepath.Join(parent, "linked")
				err = os.Symlink(path, link)
				path = link
			case "dangling-link":
				path = filepath.Join(parent, "dangling")
				err = os.Symlink(filepath.Join(parent, "absent"), path)
			case "ancestor-symlink":
				link := filepath.Join(base, "linked")
				err = os.Symlink(parent, link)
				path = filepath.Join(link, "approval.json")
			case "hardlink":
				err = os.Link(path, filepath.Join(parent, "another"))
			case "fifo":
				path = filepath.Join(parent, "fifo")
				err = syscall.Mkfifo(path, 0600)
			case "too-large":
				err = os.WriteFile(path, append(raw, []byte(strings.Repeat(" ", 4097))...), 0600)
			case "relative":
				path = "approval.json"
			case "dot-dot":
				path = parent + "/../operator/approval.json"
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("MM_REPORT_METHOD_APPROVAL", path)
			if _, err := LoadMethodApproval(card); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe approval treated as trusted or absent: %v", err)
			}
		})
	}
}
