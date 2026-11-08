package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTeamAudienceIncludesGuestAndRestrictedMembersAndDetectsChanges(t *testing.T) {
	team := strings.Repeat("t", 26)
	a := strings.Repeat("a", 26)
	b := strings.Repeat("b", 26)
	scans := 0
	change := false
	incomplete := false
	restricted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any
		switch {
		case r.URL.Path == "/api/v4/users/me":
			value = map[string]any{"id": a, "roles": "system_user"}
		case strings.HasSuffix(r.URL.Path, "/roles/name/system_user"):
			permissions := []string{"view_members"}
			if restricted {
				permissions = []string{}
			}
			value = map[string]any{"name": "system_user", "permissions": permissions}
		case strings.HasSuffix(r.URL.Path, "/stats"):
			value = map[string]any{"team_id": team, "total_member_count": 2}
		case strings.HasSuffix(r.URL.Path, "/members"):
			scans++
			role := "team_guest"
			if change && scans%2 == 0 {
				role = "team_user"
			}
			members := []map[string]any{{"team_id": team, "user_id": a, "roles": "team_user", "scheme_user": true}, {"team_id": team, "user_id": b, "roles": role, "scheme_guest": true}}
			if incomplete {
				members = members[:1]
			}
			value = members
		default:
			value = map[string]any{"id": team, "scheme_id": "scheme-1", "delete_at": 0}
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	c := New(server.URL, "test", time.Second)
	members, revision, err := c.TeamAudience(t.Context(), team)
	if err != nil || len(members) != 2 || members[1] != b || revision == "" {
		t.Fatal("guest/restricted reader lost", err)
	}
	restricted = true
	if _, _, err = c.TeamAudience(t.Context(), team); err == nil {
		t.Fatal("matching restricted pages and stats falsely proved full team")
	}
	restricted = false
	change = true
	if _, _, err = c.TeamAudience(t.Context(), team); err == nil {
		t.Fatal("same-count role/permission change accepted")
	}
	change = false
	incomplete = true
	if _, _, err = c.TeamAudience(t.Context(), team); err == nil {
		t.Fatal("incomplete team accepted")
	}
}
