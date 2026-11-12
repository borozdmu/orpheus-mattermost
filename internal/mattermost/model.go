package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// Each successful command has its own preference. Concurrent changes and replay
// cannot overwrite a newer command. Preferences belong to the authenticated bot.
type ModelSetting struct {
	Profile string `json:"profile"`
	Base    string `json:"base"`
	At      int64  `json:"at"`
	Reply   string `json:"reply"`
}
type modelPreference struct {
	UserID   string `json:"user_id"`
	Category string `json:"category"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

func (c *Client) ChatAdmin(ctx context.Context, channel Channel, user User) (bool, error) {
	if user.IsBot || user.DeleteAt != 0 {
		return false, nil
	}
	if slices.Contains(strings.Fields(user.Roles), "system_admin") {
		return true, nil
	}
	if channel.Type == "D" || channel.Type == "G" {
		return false, nil
	}
	var m struct {
		ChannelID   string `json:"channel_id"`
		UserID      string `json:"user_id"`
		Roles       string `json:"roles"`
		SchemeAdmin bool   `json:"scheme_admin"`
	}
	err := c.json(ctx, "GET", "/channels/"+channel.ID+"/members/"+user.ID, nil, &m)
	return err == nil && m.ChannelID == channel.ID && m.UserID == user.ID && (m.SchemeAdmin || slices.Contains(strings.Fields(m.Roles), "channel_admin")), err
}
func (c *Client) ModelSettings(ctx context.Context, bot, channel string) (map[string]ModelSetting, error) {
	var prefs []modelPreference
	category := "model_" + channel
	err := c.json(ctx, "GET", "/users/"+bot+"/preferences/"+category, nil, &prefs)
	if err != nil {
		return nil, err
	}
	result := make(map[string]ModelSetting, len(prefs))
	for _, pref := range prefs {
		var s ModelSetting
		if pref.UserID != bot || pref.Category != category || json.Unmarshal([]byte(pref.Value), &s) != nil || s.At <= 0 {
			return nil, errors.New("invalid stored chat model preference")
		}
		result[pref.Name] = s
	}
	return result, nil
}
func (c *Client) SaveModelSetting(ctx context.Context, bot, channel, post string, s ModelSetting) error {
	value, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.json(ctx, "PUT", "/users/"+bot+"/preferences", []modelPreference{{UserID: bot, Category: "model_" + channel, Name: post, Value: string(value)}}, nil)
}
