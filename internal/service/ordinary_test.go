package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	api "github.com/orpheus-agents/orpheus/client"
)

type ordinaryTestMM struct {
	*accessSource
	team   []string
	change bool
}

func (m *ordinaryTestMM) Audience(ctx context.Context, id string) ([]string, error) {
	ids, err := m.accessSource.Audience(ctx, id)
	if m.change && m.reads%2 == 0 {
		ids[len(ids)-1] = strings.Repeat("z", 26)
	}
	return ids, err
}
func (m *ordinaryTestMM) TeamAudience(context.Context, string) ([]string, string, error) {
	return slices.Clone(m.team), "synthetic-team-revision", nil
}

func TestOrdinaryReadsFullDestinationWithoutRoomRegistry(t *testing.T) {
	e, _, src, _, key, w := protectedFixture(t)
	w.Access = &config.Access{OrdinaryAgentUse: true, ChannelOnly: true, Namespace: "test", BotID: e.Bot.ID}
	w.IncludeIDs = nil
	w.PrivateChannels, w.DirectMessages, w.GroupMessages, w.PublicTeamAudience = true, true, true, true
	src.channel.Shared = new(false)
	src.users = map[string]mattermost.User{e.Bot.ID: {ID: e.Bot.ID, Username: "test-bot", IsBot: true}}
	e.SourceID = w.Mattermost.Source(e.Bot.ID)
	key.Source = e.SourceID
	slices.Sort(src.audience)
	mm := &ordinaryTestMM{accessSource: src, team: append(slices.Clone(src.audience), strings.Repeat("x", 26))}
	e.MM = mm
	for _, kind := range []string{"D", "G", "P", "O"} {
		t.Run(kind, func(t *testing.T) {
			src.channel.Type = kind
			v, err := e.observeOrdinary(t.Context(), w, key)
			want := src.audience
			if kind == "O" {
				want = mm.team
			}
			if err != nil || !slices.Equal(v.ids(), want) || v.Source != key.Source || v.Destination != key.Channel || !v.Complete || v.Recipients[0].Kind != "bot" {
				t.Fatalf("incorrect complete %s audience: %v", kind, err)
			}
		})
	}
	src.channel.Type = "P"
	mm.change = true
	if _, err := e.observeOrdinary(t.Context(), w, key); err == nil {
		t.Fatal("same count membership change accepted")
	}
	mm.change = false
	src.channel.Shared = nil
	if _, err := e.observeOrdinary(t.Context(), w, key); err == nil {
		t.Fatal("federation status missing")
	}
	src.channel.Shared = new(false)
	key.Source = "foreign"
	if _, err := e.observeOrdinary(t.Context(), w, key); err == nil {
		t.Fatal("foreign input source accepted")
	}
}

func TestOrdinaryAdmissionBindsPurposeAudienceDestinationAndRevocation(t *testing.T) {
	o := ordinaryObservation{Actor: strings.Repeat("a", 26), Complete: true, Destination: strings.Repeat("d", 26), ObservedAt: time.Now().Unix(), Recipients: []ordinaryRecipient{{strings.Repeat("a", 26), "human"}, {strings.Repeat("b", 26), "bot"}}, Source: strings.Repeat("a", 32)}
	for _, tc := range []struct{ mode, want string }{{"allow", ""}, {"revoked", "channel_policy_denied"}, {"changed destination", channelUnavailable}, {"wrong purpose", channelUnavailable}, {"old id", channelUnavailable}, {"null", channelUnavailable}, {"missing", channelUnavailable}} {
		t.Run(tc.mode, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					ID     string         `json:"request_id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "audience.agent_admit" || req.Params["source"] != o.Source || req.Params["destination"] != o.Destination || req.Params["actor"] != o.Actor || len(req.Params) != 6 || r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("bad ordinary authority request")
				}
				if tc.mode == "changed destination" {
					req.Params["destination"] = strings.Repeat("f", 26)
				}
				canonical, _ := json.Marshal(req.Params)
				sha := sha256.Sum256(canonical)
				result := map[string]any{"allowed": tc.mode != "revoked", "purpose": "agent_use", "observation_sha256": hex.EncodeToString(sha[:])}
				switch tc.mode {
				case "wrong purpose":
					result["purpose"] = "material_delivery"
				case "old id":
					req.ID = "prior"
				case "null":
					result["allowed"] = nil
				case "missing":
					delete(result, "purpose")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "request_id": req.ID, "result": result})
			}))
			defer s.Close()
			client := s.Client()
			client.Transport = roundTripAt{s.URL, client.Transport}
			for range 2 {
				err := ordinaryAdmission(t.Context(), o, channelCredentials{token: "synthetic"}, client)
				if tc.want == "" && err != nil {
					t.Fatal(err)
				}
				if tc.want != "" {
					if d, ok := errors.AsType[*audienceDenial](err); !ok || d.code != tc.want {
						t.Fatal("wrong refusal", err)
					}
				}
			}
			stale := o
			stale.ObservedAt -= 60
			if ordinaryAdmission(t.Context(), stale, channelCredentials{}, client) == nil || calls != 2 {
				t.Fatal("cached grant or expired observation accepted")
			}
		})
	}
}

func TestOrdinaryReplyRechecksEveryOriginalBatchAuthor(t *testing.T) {
	_, _, m, _, k, w := protectedFixture(t)
	w.Access.OrdinaryAgentUse = true
	p1 := m.posts[0]
	p2 := p1
	p2.ID = strings.Repeat("z", 26)
	p2.RootID = k.Root
	p2.UserID = strings.Repeat("v", 26)
	env := conversation.Envelope{Root: k.Root, Channel: k.Channel, Anchor: p1.ID, TriggerIDs: []string{p1.ID, p2.ID}, Versions: []conversation.Version{conversation.PostVersion(p1), conversation.PostVersion(p2)}, AccessContext: &api.AccessContext{Subject: api.AccessSubject{ID: p1.UserID}}}
	posts := []mattermost.Post{p1, p2}
	all, err := replyAuthors(w, env, posts)
	if err != nil || !slices.Equal(all, []string{p1.UserID, p2.UserID}) {
		t.Fatal("lost batch author", err)
	}
	posts[1].Message += " changed"
	if _, err = replyAuthors(w, env, posts); !errors.Is(err, errWithholdOutput) {
		t.Fatal("changed source allowed")
	}
	if _, err = replyAuthors(w, env, posts[:1]); !errors.Is(err, errWithholdOutput) {
		t.Fatal("missing source allowed")
	}
}

// ordinaryThreadFixture connects an ordinary workflow to a synthetic agent-use authority.
func ordinaryThreadFixture(t *testing.T, allowed *bool) (*Engine, *accessAPI, *accessSource, conversation.Key, config.Workflow, *int) {
	t.Helper()
	e, a, src, _, key, w := protectedFixture(t)
	w.Access = &config.Access{OrdinaryAgentUse: true, ChannelOnly: true, Namespace: "test", BotID: e.Bot.ID}
	w.IncludeIDs = nil
	w.PrivateChannels = true
	src.channel.Shared = new(false)
	src.users = map[string]mattermost.User{e.Bot.ID: {ID: e.Bot.ID, Username: "test-bot", IsBot: true}}
	slices.Sort(src.audience)
	e.SourceID = w.Mattermost.Source(e.Bot.ID)
	key.Source = e.SourceID
	t.Setenv(directAdmissionMode, "data")
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			ID     string          `json:"request_id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "audience.agent_admit" {
			t.Error("bad ordinary authority request")
		}
		sum := sha256.Sum256(req.Params)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true, "request_id": req.ID, "result": map[string]any{"allowed": *allowed, "purpose": "agent_use", "observation_sha256": hex.EncodeToString(sum[:])}})
	}))
	t.Cleanup(s.Close)
	client := s.Client()
	client.Transport = roundTripAt{s.URL, client.Transport}
	oldHTTP, oldCredentials := channelHTTP, loadChannelCredentials
	channelHTTP = client
	loadChannelCredentials = func() (channelCredentials, bool) { return channelCredentials{token: "synthetic"}, true }
	t.Cleanup(func() { channelHTTP, loadChannelCredentials = oldHTTP, oldCredentials })
	return e, a, src, key, w, &calls
}

func ordinaryRejections(src *accessSource, reason string) int {
	n := 0
	for _, p := range src.posts {
		if p.UserID == src.bot && p.Message == reason {
			n++
		}
	}
	return n
}

// An unusable mention must be consumed, or the builder selects it on every pass
// and no later mention in the same thread can start.
func TestOrdinaryUnusableInputIsConsumed(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		prepare      func(*Engine, *accessSource)
	}{
		{"attachment", ordinaryTextOnly, func(_ *Engine, s *accessSource) { s.posts[0].FileIDs = []string{strings.Repeat("f", 26)} }},
		{"oversized", ordinaryTooLarge, func(e *Engine, s *accessSource) {
			e.Config.MaxRequestBytes = 4096
			s.posts[0].Message = "@orpheus " + strings.Repeat("large ", 2000)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := true
			e, a, src, key, w, calls := ordinaryThreadFixture(t, &allowed)
			tc.prepare(e, src)
			for range 2 {
				if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
					t.Fatal(err)
				}
			}
			if len(a.attempts) != 0 || src.calls != 1 || ordinaryRejections(src, tc.reason) != 1 || *calls == 0 {
				t.Fatal("unusable input was not durably rejected once after authorization", len(a.attempts), src.calls, *calls)
			}
			if _, frozen := e.get(key); frozen {
				t.Fatal("rejected input stayed frozen")
			}
			next := strings.Repeat("n", 26)
			appendAccessPost(src, key, next, src.posts[0].UserID, "@orpheus short question", 5000)
			// The next text mention reaches local input processing, which is absent in tests.
			_ = e.Thread(t.Context(), w, key, src.channel, true)
			if p, frozen := e.get(key); !frozen || p.Envelope.Anchor != next {
				t.Fatal("later mention remained blocked by the rejected one")
			}
		})
	}
}

func TestOrdinaryRejectionRequiresAgentUseGrant(t *testing.T) {
	allowed := false
	e, a, src, key, w, _ := ordinaryThreadFixture(t, &allowed)
	src.posts[0].FileIDs = []string{strings.Repeat("f", 26)}
	if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil {
		t.Fatal(err)
	}
	if len(a.attempts) != 0 || ordinaryRejections(src, ordinaryTextOnly) != 0 || src.calls != 1 {
		t.Fatal("rejection posted without a grant, or the refusal was not noticed", src.calls)
	}
	src.fail = true
	if err := e.Thread(t.Context(), w, key, src.channel, true); err != nil || src.calls != 1 {
		t.Fatal("refused input was not consumed", err)
	}
}
