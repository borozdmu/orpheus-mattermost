package orpheus

import (
	"context"
	"slices"
	"strings"

	api "github.com/orpheus-agents/orpheus/client"
)

// A workflow may select its base profile and its explicitly provisioned variants.
// Unrelated profiles can have different credentials and are never offered.
func InModelFamily(base, name string) bool {
	return base != "" && (name == base || strings.HasPrefix(name, base+"--"))
}

func (c *Client) Profiles(ctx context.Context) ([]api.Profile, error) {
	catalog, err := decode[api.Profiles](c.api.GetProfiles(ctx)) //nolint:bodyclose // decode closes the response body.
	return catalog.Items, err
}

func SelectModel(items []api.Profile, base, current string, args []string) (profile, message string) {
	var options []string
	active := "не настроена"
	for _, p := range items {
		if !InModelFamily(base, p.Name) || p.Model == nil || *p.Model == "" {
			continue
		}
		effort := "default"
		if p.Codex.Effort != nil {
			effort = string(*p.Codex.Effort)
		}
		choice := *p.Model + " " + effort
		options = append(options, choice)
		if p.Name == current {
			active = choice
		}
		if len(args) == 2 && args[0] == *p.Model && args[1] == effort && (profile == "" || p.Name == base) {
			profile = p.Name
		}
	}
	if profile != "" {
		return profile, "Для новых разговоров в этом чате установлено: " + strings.Join(args, " ")
	}
	if len(options) == 0 {
		return "", "Ядро не вернуло доступных моделей для этого бота."
	}
	slices.Sort(options)
	hint := "Текущая настройка: " + active + ". Доступные сочетания модели и усилия:\n" + strings.Join(slices.Compact(options), "\n") + "\nФормат: /model <модель> <усилие>."
	if len(args) != 0 {
		hint = "Такого сочетания нет. " + hint
	}
	return "", hint
}
