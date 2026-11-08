package mattermost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

// TeamAudience conservatively includes ALL current team members, including
// guests and restricted accounts. Permissions never remove a reader here.
// Two complete scans also detect same-count membership/role/scheme changes.
func (c *Client) TeamAudience(ctx context.Context, team string) ([]string, string, error) {
	denied := errors.New("complete current team membership unavailable")
	if !config.ValidID(team) {
		return nil, "", denied
	}
	type member struct {
		TeamID      string `json:"team_id"`
		UserID      string `json:"user_id"`
		Roles       string `json:"roles"`
		DeleteAt    int64  `json:"delete_at"`
		SchemeGuest bool   `json:"scheme_guest"`
		SchemeUser  bool   `json:"scheme_user"`
		SchemeAdmin bool   `json:"scheme_admin"`
	}
	scan := func() ([]string, string, error) {
		permissions, err := c.unrestrictedObserver(ctx)
		if err != nil {
			return nil, "", err
		}
		var info struct {
			ID       string `json:"id"`
			DeleteAt int64  `json:"delete_at"`
			SchemeID string `json:"scheme_id"`
		}
		if c.json(ctx, "GET", "/teams/"+url.PathEscape(team), nil, &info) != nil || info.ID != team || info.DeleteAt != 0 {
			return nil, "", denied
		}
		count := func() (int, error) {
			var stats struct {
				TeamID string `json:"team_id"`
				Count  *int   `json:"total_member_count"`
			}
			err := c.json(ctx, "GET", "/teams/"+url.PathEscape(team)+"/stats", nil, &stats)
			if err != nil || stats.TeamID != team || stats.Count == nil || *stats.Count < 1 || *stats.Count > 4096 {
				return 0, denied
			}
			return *stats.Count, nil
		}
		before, err := count()
		if err != nil {
			return nil, "", err
		}
		seen := map[string]bool{}
		var all []member
		var ids []string
		for page := 0; ; page++ {
			var batch []member
			q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"200"}}
			if c.json(ctx, "GET", "/teams/"+url.PathEscape(team)+"/members?"+q.Encode(), nil, &batch) != nil || batch == nil || len(batch) > 200 {
				return nil, "", denied
			}
			for _, m := range batch {
				if m.TeamID != team || !config.ValidID(m.UserID) || seen[m.UserID] || m.DeleteAt != 0 {
					return nil, "", denied
				}
				seen[m.UserID] = true
				all = append(all, m)
				ids = append(ids, m.UserID)
			}
			if len(all) > before {
				return nil, "", denied
			}
			if len(batch) < 200 {
				break
			}
		}
		after, err := count()
		if err != nil || after != before || len(all) != after {
			return nil, "", denied
		}
		slices.Sort(ids)
		slices.SortFunc(all, func(a, b member) int {
			if a.UserID < b.UserID {
				return -1
			}
			if a.UserID > b.UserID {
				return 1
			}
			return 0
		})
		raw, _ := json.Marshal(struct {
			Team                any
			Members             []member
			ObserverPermissions string
		}{info, all, permissions})
		sum := sha256.Sum256(raw)
		return ids, hex.EncodeToString(sum[:]), nil
	}
	first, revision, err := scan()
	if err != nil {
		return nil, "", err
	}
	second, next, err := scan()
	if err != nil || revision != next || !slices.Equal(first, second) {
		return nil, "", denied
	}
	return first, revision, nil
}

// GetViewUsersRestrictions is nil only when effective global view_members is
// granted. Counts and member pages alone may share the same hidden subset.
func (c *Client) unrestrictedObserver(ctx context.Context) (string, error) {
	denied := errors.New("unrestricted team observer permission is not proven")
	var me User
	if c.json(ctx, "GET", "/users/me", nil, &me) != nil || !config.ValidID(me.ID) || me.DeleteAt != 0 {
		return "", denied
	}
	roles := strings.Fields(me.Roles)
	if len(roles) == 0 || len(roles) > 32 {
		return "", denied
	}
	permission := false
	definitions := map[string][]string{}
	for _, name := range roles {
		if !regexp.MustCompile(`^[a-z0-9_]{1,64}$`).MatchString(name) {
			return "", denied
		}
		var role struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
			DeleteAt    int64    `json:"delete_at"`
		}
		if c.json(ctx, "GET", "/roles/name/"+url.PathEscape(name), nil, &role) != nil || role.Name != name || role.DeleteAt != 0 || role.Permissions == nil || len(role.Permissions) > 4096 {
			return "", denied
		}
		slices.Sort(role.Permissions)
		definitions[name] = role.Permissions
		permission = permission || slices.Contains(role.Permissions, "view_members")
	}
	if !permission {
		return "", denied
	}
	raw, _ := json.Marshal(struct {
		Observer string
		Roles    map[string][]string
	}{me.ID, definitions})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
