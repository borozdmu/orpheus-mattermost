package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

func TestAccountDirectoryConfirmsRealBooleanWithoutRecordingTheAssignment(t *testing.T) {
	const actor = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const trace = "cd12ab34cd12ab34cd12ab34cd12ab34"
	access := &config.Access{VerifyAccountCards: true, BotID: "bot", ScopeContext: map[string]any{"agent_id": "mark"}, IdentityProjection: config.IdentityProjection{Subjects: map[string]config.SubjectID{actor: config.SubjectID("7")}}}
	var reply, received, rid string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			RequestID string `json:"request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		rid = q.RequestID
		received = r.Header.Get(directoryProbeHeader)
		_, _ = fmt.Fprintf(w, reply, q.RequestID)
	}))
	defer srv.Close()
	prev := accountDirectoryHTTP
	accountDirectoryHTTP = srv.Client()
	t.Cleanup(func() { accountDirectoryHTTP = prev })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", srv.URL)
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("s", 32))
	for _, tc := range []struct {
		body    string
		want    string
		allowed bool
	}{
		{`{"request_id":"%s","ok":true,"result":true}`, "allow", true},
		{`{"result":false,"ok":true,"request_id":"%s"}`, "deny", false},
		{`{"ok":true,"request_id":"%s"}`, "", false},
		{`{"request_id":"%s","ok":true,"result":null}`, "", false},
		{`{"request_id":"%s","ok":true,"result":false,"result":true}`, "", false},
		{`{"request_id":"%s","ok":true,"result":false,"error":{"code":"denied"}}`, "", false},
		{`{"request_id":"other-%s","ok":true,"result":false}`, "", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			reply = tc.body
			out, err := os.CreateTemp(t.TempDir(), "events")
			if err != nil {
				t.Fatal(err)
			}
			prevOut := os.Stdout
			os.Stdout = out
			t.Cleanup(func() { os.Stdout = prevOut; _ = out.Close() })
			real := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := confirmAccountCards(r.Context(), access, []string{"bot", actor}); err != nil {
					http.Error(w, "audience unconfirmed", http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			r := httptest.NewRequest(http.MethodPost, "/internal/audience/confirm", strings.NewReader("private signed assignment text"))
			r.Header.Set(directoryProbeHeader, trace)
			rec := httptest.NewRecorder()
			withDirectoryProbe(real).ServeHTTP(rec, r)
			if received != trace || (rec.Code == http.StatusOK) != tc.allowed {
				t.Fatal("route or result mismatch")
			}
			raw, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			n := 1
			if tc.want != "" {
				n = 2
			}
			if len(lines) != n {
				t.Fatalf("unexpected number of events %d", len(lines))
			}
			for i, line := range lines {
				var obj map[string]string
				if json.Unmarshal([]byte(line), &obj) != nil {
					t.Fatal("invalid event")
				}
				if obj["kind"] != "kord.route.v1" || obj["trace"] != trace || obj["request_id"] != rid || rid == "" {
					t.Fatal("event mismatch")
				}
				if i == 0 && (len(obj) != 4 || obj["stage"] != "rpc") {
					t.Fatal("invalid rpc")
				}
				if i == 1 && (len(obj) != 5 || obj["stage"] != "decision" || obj["decision"] != tc.want) {
					t.Fatal("invalid decision")
				}
			}
		})
	}
}

func TestSharedAccountDirectoryRejectsCompetingEndpoint(t *testing.T) {
	t.Setenv("KORD_PROVIDER_ENDPOINT_MODE", "shared-file")
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", "https://old.invalid")
	folder := t.TempDir()
	if _, err := accountDirectoryURL(folder); err == nil {
		t.Fatal("legacy URL accepted")
	}
	t.Setenv("KORD_PROVIDER_ENDPOINT_MODE", "wrong")
	if _, err := accountDirectoryURL(folder); err == nil {
		t.Fatal("invalid mode")
	}
	if _, err := sharedProviderURL(folder); err == nil {
		t.Fatal("missing file accepted")
	}
	path := filepath.Join(folder, "url")
	if err := os.WriteFile(path, []byte("http://127.0.0.1:18107\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sharedProviderURL(folder); err == nil {
		t.Fatal("writable file accepted")
	}
	if err := os.Chmod(path, 0444); err != nil {
		t.Fatal(err)
	}
	out, err := sharedProviderURL(folder)
	if os.Geteuid() == 0 && (err != nil || out != "http://127.0.0.1:18107") {
		t.Fatal("root-owned address rejected")
	}
	if os.Geteuid() != 0 && err == nil {
		t.Fatal("writable user-owned directory accepted")
	}
}

func TestDirectoryProbeHeaderDoesNotAlterUntaggedContext(t *testing.T) {
	seen := "initial"
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = r.Context().Value(directoryProbeKey{}).(string)
	})
	for _, headers := range [][]string{nil, {"bad"}, {strings.Repeat("a", 32), strings.Repeat("b", 32)}} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/audience/confirm", nil)
		for _, v := range headers {
			r.Header.Add(directoryProbeHeader, v)
		}
		withDirectoryProbe(next).ServeHTTP(httptest.NewRecorder(), r)
		if seen != "" {
			t.Fatal("ambiguous trace accepted")
		}
	}
}
