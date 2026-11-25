package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

// localSource adds the current-post reads used by local input projection.
type localSource struct{ *accessSource }

func (s localSource) Post(_ context.Context, id string) (mattermost.Post, error) {
	for _, p := range s.posts {
		if p.ID == id {
			return p, nil
		}
	}
	return mattermost.Post{}, errors.New("post not found")
}
func (s localSource) File(context.Context, string) (mattermost.FileInfo, error) {
	return mattermost.FileInfo{}, errors.New("files are not used here")
}
func (s localSource) Download(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("files are not used here")
}

// localProviderThread wires a local-provider binding with a fixed peer to a
// synthetic host policy (/admit) and local input processor (/project).
func localProviderThread(t *testing.T, continued bool) (*Engine, *accessAPI, *accessSource, conversation.Key, config.Workflow, *string, *int, *int) {
	t.Helper()
	e, a, src, _, key, w := protectedFixture(t)
	const peer = "pppppppppppppppppppppppppp"
	w.Access.Peer = &config.PeerRecipient{ID: peer, Field: "peer_id"}
	w.Access.LocalServices, w.Access.LocalProviderContext = true, true
	w.Access.ContinueThreadSessions = continued
	w.Access.ScopeContext["policy_space_id"] = "test." + peer
	w.Services = []string{"kord"}
	src.audience = append(src.audience, "nnnnnnnnnnnnnnnnnnnnnnnnnn", peer)
	src.users = map[string]mattermost.User{peer: {ID: peer, IsBot: true}}
	e.MM = localSource{src}

	pin := strings.Repeat("a", 64)
	decision, admissions, projections := "", 0, 0
	policy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		admissions++
		var req struct {
			ID     string `json:"request_id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "channel.admit" {
			t.Error("bad admission request")
		}
		result := map[string]any{"binding": pin, "reason": nil, "participants": []string{}}
		if decision != "" {
			result["reason"] = decision
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true, "result": result, "request_id": req.ID})
	}))
	t.Cleanup(policy.Close)
	client := policy.Client()
	client.Transport = roundTripAt{policy.URL, client.Transport}
	input := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		projections++
		body, _ := io.ReadAll(r.Body)
		_, _ = rw.Write([]byte("projected: " + string(body)))
	}))
	t.Cleanup(input.Close)

	t.Setenv(directAdmissionMode, "data")
	oldHTTP, oldCredentials, oldInput := channelHTTP, loadChannelCredentials, localInputTransport
	channelHTTP = client
	loadChannelCredentials = func() (channelCredentials, bool) { return channelCredentials{token: "synthetic", binding: pin}, true }
	localInputTransport = func() *http.Transport {
		return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", input.Listener.Addr().String())
		}}
	}
	t.Cleanup(func() { channelHTTP, loadChannelCredentials, localInputTransport = oldHTTP, oldCredentials, oldInput })
	return e, a, src, key, w, &decision, &admissions, &projections
}

func TestLocalProviderThreadContinuesOnlyWithFlag(t *testing.T) {
	for _, continued := range []bool{false, true} {
		t.Run(map[bool]string{false: "single run", true: "continued"}[continued], func(t *testing.T) {
			e, a, src, key, w, _, admissions, projections := localProviderThread(t, continued)
			if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
				t.Fatal(err)
			}
			if len(a.attempts) != 1 || a.attempts[0].Session != "" {
				t.Fatal("first mention did not start a session", len(a.attempts))
			}
			if a.snapshot.Sessions[0].AllowMultipleRuns != continued {
				t.Fatal("session run policy does not follow the flag")
			}
			completeAccessRun(a)
			firstAdmissions, firstProjections := *admissions, *projections
			appendAccessPost(src, key, "ssssssssssssssssssssssssss", src.audience[1], "@orpheus SECOND_QUESTION", 4000)
			if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
				t.Fatal(err)
			}
			if len(a.attempts) != 2 {
				t.Fatal("second mention was not submitted", len(a.attempts))
			}
			second := a.attempts[1]
			if continued != (second.Session == a.snapshot.Sessions[0].ID) || !continued && second.Session != "" {
				t.Fatal("second mention used the wrong session", second.Session)
			}
			input := joinedInput(second.Messages)
			if !strings.Contains(input, "SECOND_QUESTION") || strings.Contains(input, "ORIGINAL_REQUEST_BODY") || !strings.Contains(input, "projected: ") {
				t.Fatal("second run did not receive only the projected new mention", input)
			}
			if *admissions <= firstAdmissions || *projections <= firstProjections {
				t.Fatal("host admission or local projection skipped on the second mention")
			}
			if second.Envelope.AccessContext == nil || second.Envelope.AccessContext.InvocationID == a.attempts[0].Envelope.AccessContext.InvocationID {
				t.Fatal("second mention reused the first admission")
			}
			if len(second.Envelope.Request.Files) != 0 {
				t.Fatal("original files reached the sandbox")
			}
		})
	}
}

func TestContinuedLocalThreadRechecksAdmissionEveryMention(t *testing.T) {
	e, a, src, key, w, decision, _, _ := localProviderThread(t, true)
	if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	*decision = "participant_access_unconfirmed"
	appendAccessPost(src, key, "ssssssssssssssssssssssssss", src.audience[1], "@orpheus after revocation", 4000)
	_ = e.Thread(t.Context(), w, key, src.channel, true)
	if len(a.attempts) != 1 {
		t.Fatal("revoked participant reached the continued session")
	}
}

func TestContinuedLocalThreadResetsOnScopeChange(t *testing.T) {
	e, a, src, key, w, _, _, _ := localProviderThread(t, true)
	if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
		t.Fatal(err)
	}
	completeAccessRun(a)
	appendAccessPost(src, key, "ssssssssssssssssssssssssss", src.audience[1], "@orpheus next", 4000)
	a.failure = &orpheus.Error{Status: 409, Code: "service_scope_changed"}
	if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
		t.Fatal(err)
	}
	last := a.attempts[len(a.attempts)-1]
	if a.attempts[1].Session == "" || last.Session != "" || strings.Contains(joinedInput(last.Messages), "ORIGINAL_REQUEST_BODY") {
		t.Fatal("scope change did not start a clean session")
	}
}
