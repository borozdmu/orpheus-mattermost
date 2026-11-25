package mattermost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
			value = map[string]any{"team_id": team, "total_member_count": 2, "active_member_count": 2}
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

func TestTeamAudienceUsesAllActiveMembersWhenInactiveUsersRemainInTeam(t *testing.T) {
	team := strings.Repeat("t", 26)
	a, b, inactive := strings.Repeat("a", 26), strings.Repeat("b", 26), strings.Repeat("i", 26)
	pages := 0
	c := teamAudienceTestClient(t, team, func(r *http.Request) any {
		if strings.HasSuffix(r.URL.Path, "/stats") {
			return map[string]any{"team_id": team, "total_member_count": 3, "active_member_count": 2}
		}
		pages++
		members := []map[string]any{teamAudienceTestMember(team, a), teamAudienceTestMember(team, b)}
		// Mattermost keeps deactivated users in total_member_count and in an
		// unfiltered member listing. Only the active-user filter removes them.
		if r.URL.Query().Get("exclude_deleted_users") != "true" {
			members = append(members, teamAudienceTestMember(team, inactive))
		}
		return members
	})
	members, revision, err := c.TeamAudience(t.Context(), team)
	if err != nil || !slices.Equal(members, []string{a, b}) || revision == "" || pages != 2 {
		t.Fatalf("active audience=%v revision=%q scans=%d err=%v", members, revision, pages, err)
	}
}

func TestTeamAudienceRequiresAnExplicitValidActiveCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"missing", nil},
		{"null", json.RawMessage("null")},
		{"zero", 0},
		{"negative", -1},
		{"oversized", 4097},
		{"boolean", false},
		{"fractional", 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team := strings.Repeat("t", 26)
			pages := 0
			c := teamAudienceTestClient(t, team, func(r *http.Request) any {
				if strings.HasSuffix(r.URL.Path, "/stats") {
					stats := map[string]any{"team_id": team, "total_member_count": 2}
					if tc.name != "missing" {
						stats["active_member_count"] = tc.value
					}
					return stats
				}
				pages++
				return []map[string]any{teamAudienceTestMember(team, strings.Repeat("a", 26)), teamAudienceTestMember(team, strings.Repeat("b", 26))}
			})
			if _, _, err := c.TeamAudience(t.Context(), team); err == nil {
				t.Fatal("unproven active count admitted a team")
			}
			if pages != 0 {
				t.Fatalf("enumerated %d pages before proving active count", pages)
			}
		})
	}
}

func TestTeamAudienceRejectsActiveMembershipChanges(t *testing.T) {
	for _, name := range []string{"deactivation_between_scans", "reactivation_between_scans", "same_count_replacement", "deactivation_during_scan"} {
		t.Run(name, func(t *testing.T) {
			team := strings.Repeat("t", 26)
			a, b, replacement := strings.Repeat("a", 26), strings.Repeat("b", 26), strings.Repeat("c", 26)
			stats, pages := 0, 0
			c := teamAudienceTestClient(t, team, func(r *http.Request) any {
				if strings.HasSuffix(r.URL.Path, "/stats") {
					stats++
					active := 2
					if name == "deactivation_between_scans" && stats > 2 || name == "reactivation_between_scans" && stats <= 2 || name == "deactivation_during_scan" && stats > 1 {
						active = 1
					}
					return map[string]any{"team_id": team, "total_member_count": 3, "active_member_count": active}
				}
				pages++
				members := []map[string]any{teamAudienceTestMember(team, a), teamAudienceTestMember(team, b)}
				if name == "deactivation_between_scans" && pages == 2 || name == "reactivation_between_scans" && pages == 1 {
					members = members[:1]
				}
				if name == "same_count_replacement" && pages == 2 {
					members[1] = teamAudienceTestMember(team, replacement)
				}
				return members
			})
			if _, _, err := c.TeamAudience(t.Context(), team); err == nil {
				t.Fatal("changed active membership accepted")
			}
			wantPages := 2
			if name == "deactivation_during_scan" {
				wantPages = 1
			}
			if pages != wantPages {
				t.Fatalf("scans=%d, want %d", pages, wantPages)
			}
		})
	}
}

func TestTeamAudienceChecksEveryFilteredPage(t *testing.T) {
	for _, name := range []string{"complete", "truncated_first_page", "hidden_last_page", "duplicate_across_pages", "deleted_membership"} {
		t.Run(name, func(t *testing.T) {
			team := strings.Repeat("t", 26)
			all := make([]map[string]any, 201)
			for i := range all {
				all[i] = teamAudienceTestMember(team, fmt.Sprintf("%026d", i))
			}
			pages := []int{}
			c := teamAudienceTestClient(t, team, func(r *http.Request) any {
				if strings.HasSuffix(r.URL.Path, "/stats") {
					return map[string]any{"team_id": team, "total_member_count": 215, "active_member_count": len(all)}
				}
				if r.URL.Query().Get("exclude_deleted_users") != "true" || r.URL.Query().Get("per_page") != "200" {
					t.Errorf("unfiltered/incomplete enumeration query: %s", r.URL.RawQuery)
				}
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				if err != nil || page < 0 || page > 1 {
					t.Errorf("unexpected page: %s", r.URL.RawQuery)
					return []map[string]any{}
				}
				pages = append(pages, page)
				if page == 0 {
					if name == "truncated_first_page" {
						return all[:199]
					}
					if name == "deleted_membership" {
						all[0]["delete_at"] = 1
					}
					return all[:200]
				}
				if name == "hidden_last_page" {
					return []map[string]any{}
				}
				if name == "duplicate_across_pages" {
					return all[:1]
				}
				return all[200:]
			})
			members, revision, err := c.TeamAudience(t.Context(), team)
			if name == "complete" {
				if err != nil || len(members) != len(all) || revision == "" || !slices.Equal(pages, []int{0, 1, 0, 1}) {
					t.Fatalf("members=%d pages=%v revision=%q err=%v", len(members), pages, revision, err)
				}
			} else if err == nil {
				t.Fatal("incomplete or inconsistent active enumeration accepted")
			}
		})
	}
}

func teamAudienceTestMember(team, user string) map[string]any {
	return map[string]any{"team_id": team, "user_id": user, "roles": "team_user", "scheme_user": true}
}

func teamAudienceTestClient(t *testing.T, team string, response func(*http.Request) any) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any
		switch r.URL.Path {
		case "/api/v4/users/me":
			value = map[string]any{"id": strings.Repeat("a", 26), "roles": "system_user"}
		case "/api/v4/roles/name/system_user":
			value = map[string]any{"name": "system_user", "permissions": []string{"view_members"}}
		case "/api/v4/teams/" + team:
			value = map[string]any{"id": team, "scheme_id": "scheme-1", "delete_at": 0}
		case "/api/v4/teams/" + team + "/stats", "/api/v4/teams/" + team + "/members":
			value = response(r)
		default:
			t.Errorf("unexpected audience request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	return New(server.URL, "test", time.Second)
}
