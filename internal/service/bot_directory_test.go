package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBotDirectoryProofRequiresExactTupleAndBooleanDecision(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		allow      bool
	}{
		{"allow", `{"request_id":"$ID","ok":true,"result":true}`, 200, true},
		{"denied", `{"request_id":"$ID","ok":true,"result":false}`, 200, false},
		{"unsupported", `{"error":"method not supported"}`, 404, false},
		{"credential not allowed", `{"error":"forbidden"}`, 403, false},
		{"unavailable", `{"request_id":"$ID","ok":true,"result":true}`, 503, false},
		{"redirect", `{"request_id":"$ID","ok":true,"result":true}`, 302, false},
		{"null", `{"request_id":"$ID","ok":true,"result":null}`, 200, false},
		{"string boolean", `{"request_id":"$ID","ok":true,"result":"true"}`, 200, false},
		{"missing result", `{"request_id":"$ID","ok":true}`, 200, false},
		{"wrong request", `{"request_id":"other-$ID","ok":true,"result":true}`, 200, false},
		{"not successful", `{"request_id":"$ID","ok":false,"result":true}`, 200, false},
		{"duplicate result", `{"request_id":"$ID","ok":true,"result":false,"result":true}`, 200, false},
		{"unknown field", `{"request_id":"$ID","ok":true,"result":true,"details":null}`, 200, false},
		{"second decision", `{"request_id":"$ID","ok":true,"result":true} {}`, 200, false},
		{"oversized", strings.Repeat(" ", 4097) + `{"request_id":"$ID","ok":true,"result":true}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, mm, _, _, w := protectedFixture(t)
			w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req struct {
					ID     string `json:"request_id"`
					Method string `json:"method"`
					Params struct {
						Provider string `json:"provider"`
						Agent    string `json:"agent_key"`
						Bots     map[string]struct {
							ID    int `json:"bot_id"`
							Owner int `json:"owner_user_id"`
						} `json:"bots"`
					} `json:"params"`
				}
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if decoder.Decode(&req) != nil || req.ID == "" || req.Method != "external.confirm_bot_audience" || req.Params.Provider != "mattermost" || req.Params.Agent != "mark" || len(req.Params.Bots) != 1 || req.Params.Bots[registeredBotOne].ID != 71 || req.Params.Bots[registeredBotOne].Owner != 99 || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("d", 32) {
					t.Error("proof must carry only the exact currently observed source/bot/owner tuple")
					http.Error(rw, "invalid", http.StatusBadRequest)
					return
				}
				if tc.status == http.StatusFound {
					rw.Header().Set("Location", "/must-not-follow")
				}
				rw.WriteHeader(tc.status)
				_, _ = rw.Write([]byte(strings.ReplaceAll(tc.body, "$ID", req.ID)))
			}))
			defer server.Close()
			previous := accountDirectoryHTTP
			accountDirectoryHTTP = server.Client()
			accountDirectoryHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			t.Cleanup(func() { accountDirectoryHTTP = previous })
			t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", server.URL)
			t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
			err := confirmBotRecipients(t.Context(), w.Access, []string{mm.bot, mm.posts[0].UserID, registeredBotOne})
			if (err == nil) != tc.allow || calls.Load() != 1 {
				t.Fatalf("allow=%v err=%v calls=%d", tc.allow, err, calls.Load())
			}
		})
	}
}

type botDirectoryFailureTransport func(*http.Request) (*http.Response, error)

func (f botDirectoryFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBotDirectoryTimeoutAndMissingCapabilityNeverFallBack(t *testing.T) {
	_, _, mm, _, _, w := protectedFixture(t)
	w.Access = registeredBotAccess(t, mm.bot, mm.posts[0].UserID)
	var calls atomic.Int32
	previous := accountDirectoryHTTP
	accountDirectoryHTTP = &http.Client{Transport: botDirectoryFailureTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.DeadlineExceeded })}
	t.Cleanup(func() { accountDirectoryHTTP = previous })
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_URL", "https://directory.invalid/internal/v1/call")
	t.Setenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN", strings.Repeat("d", 32))
	if confirmBotRecipients(t.Context(), w.Access, []string{mm.bot, mm.posts[0].UserID, registeredBotOne}) == nil || calls.Load() != 1 {
		t.Fatal("unavailable recipient proof fell back to configuration")
	}
	if err := confirmBotRecipients(t.Context(), w.Access, []string{mm.bot, mm.posts[0].UserID}); err != nil || calls.Load() != 1 {
		t.Fatal("absent registered bot requires no recipient proof", err)
	}
	if err := confirmBotRecipients(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Access.BotRecipients = nil
	if err := confirmBotRecipients(t.Context(), w.Access, nil); err != nil || calls.Load() != 1 {
		t.Fatal("legacy binding gained an extra directory dependency", err)
	}
}
