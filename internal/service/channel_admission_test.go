package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestDirectChannelAdmissionRechecksEachAttemptAndPinsClient(t *testing.T) {
	token, pin := strings.Repeat("k", 32), strings.Repeat("a", 64)
	actor, bot, peer := strings.Repeat("a", 26), strings.Repeat("b", 26), strings.Repeat("p", 26)
	request := channelRequest{Workflow: "one", Channel: strings.Repeat("c", 26), Bot: bot, Audience: []string{actor, bot, peer}, Authors: []string{actor}}
	a := &config.Access{BotID: bot, IdentityProjection: config.IdentityProjection{Subjects: map[string]config.SubjectID{actor: nil}}}
	c := channelCredentials{token, pin}
	for _, tc := range []struct{ name, mode, want string }{{"allowed", "allow", ""}, {"revoked", "revoke", "participant_access_unconfirmed"}, {"different audience", "deny", "channel_policy_denied"}, {"different credential", "unauthorized", channelUnavailable}, {"stale decision", "stale", channelUnavailable}, {"foreign pin", "pin", channelUnavailable}, {"foreign participant", "foreign", channelUnavailable}, {"bad shape", "extra", channelUnavailable}, {"service failure", "unavailable", channelUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/call" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect authority call")
				}
				var req struct {
					ID     string         `json:"request_id"`
					Method string         `json:"method"`
					Params channelRequest `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" || req.Method != "channel.admit" || req.Params.Workflow != request.Workflow || !slices.Equal(req.Params.Audience, request.Audience) {
					t.Error("incorrect authority request")
				}
				if tc.mode == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				if tc.mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				result := map[string]any{"binding": pin, "reason": nil, "participants": []string{}}
				switch tc.mode {
				case "revoke":
					result["reason"] = "participant_access_unconfirmed"
					result["participants"] = []string{actor}
				case "deny":
					result["reason"] = "channel_policy_denied"
				case "stale":
					req.ID = "stale"
				case "pin":
					result["binding"] = strings.Repeat("b", 64)
				case "foreign":
					result["reason"] = "identity_unconfirmed"
					result["participants"] = []string{peer}
				case "extra":
					result["extra"] = "untrusted private detail"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result, "request_id": req.ID})
			}))
			defer server.Close()
			client := server.Client()
			client.Transport = roundTripAt{server.URL, client.Transport}
			for range 2 {
				err := directGate(t.Context(), request, a, c, client)
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if d, ok := errors.AsType[*audienceDenial](err); !ok || d.code != tc.want {
					t.Fatalf("incorrect decision: %v", err)
				}
			}
			if requests != 2 {
				t.Fatal("decision was cached or caller retried")
			}
		})
	}
}

type roundTripAt struct {
	target    string
	transport http.RoundTripper
}

func (s roundTripAt) RoundTrip(r *http.Request) (*http.Response, error) {
	v := r.Clone(r.Context())
	v.URL.Host = strings.TrimPrefix(s.target, "http://")
	return s.transport.RoundTrip(v)
}

func TestChannelAdmissionMalformedEvidence(t *testing.T) {
	pin := strings.Repeat("0", 64)
	good := `{"ok":true,"request_id":"id","result":{"binding":"` + pin + `","reason":null,"participants":[]}}`
	for _, v := range []string{good + good, strings.Replace(good, `"ok":true`, `"ok":false,"ok":true`, 1), strings.Replace(good, `"reason":null`, `"reason":null,"reason":null`, 1), strings.Replace(good, `"reason":null,`, "", 1), strings.Replace(good, `"participants":[]`, `"participants":null`, 1), strings.Replace(good, `"reason":null`, `"reason":""`, 1), strings.Replace(good, `"ok":true`, `"OK":true`, 1)} {
		if d, ok := errors.AsType[*audienceDenial](channelDecision([]byte(v), "id", pin, &config.Access{})); !ok || d.code != channelUnavailable {
			t.Fatal("accepted invalid response")
		}
	}
	if _, ok := decodeCredentials([]byte(`{"binding":"` + pin + `","token":"` + strings.Repeat("x", 32) + `"}`)); !ok {
		t.Fatal("valid credential rejected")
	}
	for _, s := range []string{`{}`, `{"binding":"a","token":"x"}`, `{"binding":"` + pin + `","token":"` + strings.Repeat("x", 31) + ` "}`, `{"binding":"` + pin + `","token":"` + strings.Repeat("x", 32) + `","token":"` + strings.Repeat("y", 32) + `"}`} {
		if _, ok := decodeCredentials([]byte(s)); ok {
			t.Fatal("untrusted credential accepted")
		}
	}
}

func TestChannelAdmissionProbeUsesSelectedWorkflowOnly(t *testing.T) {
	t.Setenv(directAdmissionMode, "data")
	chat, bot := strings.Repeat("c", 26), strings.Repeat("b", 26)
	w := config.Workflow{ID: "wf", Access: &config.Access{ChannelOnly: true, BotID: bot}, IncludeIDs: []string{chat}}
	cfg := config.Config{Workflows: []config.Workflow{w}}
	list, err := ChannelAdmissionBindings(cfg)
	if err != nil || len(list) != 1 || list["wf"] != chat {
		t.Fatal("wrong binding list")
	}
	trace := strings.Repeat("a", 32)
	req := channelRequest{Workflow: "wf", Channel: chat, Bot: bot, Audience: []string{}, Authors: []string{}}
	check := func(ctx context.Context, p channelRequest, out io.Writer) error {
		raw, _ := json.Marshal(p)
		return CheckChannelAdmission(ctx, cfg, trace, bytes.NewReader(raw), out)
	}
	var out bytes.Buffer
	if err = check(t.Context(), req, &out); err != nil || !strings.Contains(out.String(), `"status":503`) {
		t.Fatal("missing credentials must be unavailable")
	}
	req.Bot = strings.Repeat("x", 26)
	if check(t.Context(), req, io.Discard) == nil {
		t.Fatal("checked unregistered bot")
	}
	cfg.Workflows = append(cfg.Workflows, w)
	if _, err = ChannelAdmissionBindings(cfg); err == nil {
		t.Fatal("ambiguous binding")
	}
	t.Setenv(directAdmissionMode, "typo")
	if err = localGate(t.Context(), w, conversation.Envelope{Channel: chat}, nil, nil); err == nil {
		t.Fatal("unknown transport")
	}
}
