package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	api "github.com/orpheus-agents/orpheus/client"
)

type chatModels interface {
	ChatAdmin(context.Context, mattermost.Channel, mattermost.User) (bool, error)
	ModelSettings(context.Context, string, string) (map[string]mattermost.ModelSetting, error)
	SaveModelSetting(context.Context, string, string, string, mattermost.ModelSetting) error
}

func modelArgs(p mattermost.Post, channel mattermost.Channel, bot mattermost.User) ([]string, bool) {
	if p.ChannelID != channel.ID || p.UserID == bot.ID || p.DeleteAt != 0 || p.Type != "" || p.Props["from_webhook"] != nil {
		return nil, false
	}
	fields := strings.Fields(p.Message)
	if len(fields) > 0 && strings.EqualFold(fields[0], "@"+bot.Username) {
		fields = fields[1:]
	} else if channel.Type != "D" {
		return nil, false
	}
	if len(fields) == 0 || fields[0] != "/model" {
		return nil, false
	}
	return fields[1:], true
}

func selectedChatModel(settings map[string]mattermost.ModelSetting, base string, before int64, anchor string) string {
	latest := int64(0)
	latestID, profile := "", base
	for id, s := range settings {
		if s.Base != base || !orpheus.InModelFamily(base, s.Profile) || s.At > before || (s.At == before && id >= anchor) {
			continue
		}
		if s.At > latest || (s.At == latest && id > latestID) {
			latest, latestID, profile = s.At, id, s.Profile
		}
	}
	return profile
}

func (e *Engine) modelReply(ctx context.Context, store chatModels, w config.Workflow, channel mattermost.Channel, post mattermost.Post, args []string) (string, error) {
	user, err := e.MM.User(ctx, post.UserID)
	if err != nil {
		return "", err
	}
	if user.ID != post.UserID || user.IsBot || post.EditAt != 0 {
		return "Команду нужно отправить новым сообщением от администратора чата.", nil
	}
	admin, err := store.ChatAdmin(ctx, channel, user)
	if err != nil {
		return "", err
	}
	if !admin {
		return "Команда доступна администратору этого чата.", nil
	}
	settings, err := store.ModelSettings(ctx, e.Bot.ID, channel.ID)
	if err != nil {
		return "", err
	}
	if previous, ok := settings[post.ID]; ok {
		return previous.Reply, nil
	}
	catalog, ok := e.API.(interface {
		Profiles(context.Context) ([]api.Profile, error)
	})
	if !ok {
		return "", errors.New("model catalog unavailable")
	}
	items, err := catalog.Profiles(ctx)
	if err != nil {
		return "Не удалось получить каталог моделей. Повторите команду позже.", nil //nolint:nilerr // Report the catalog outage as a service reply.
	}
	current := selectedChatModel(settings, w.Profile, post.CreateAt, post.ID)
	profile, reply := orpheus.SelectModel(items, w.Profile, current, args)
	if profile != "" {
		err = store.SaveModelSetting(ctx, e.Bot.ID, channel.ID, post.ID, mattermost.ModelSetting{Profile: profile, Base: w.Profile, At: post.CreateAt, Reply: reply})
	}
	return reply, err
}

func (e *Engine) modelCommands(ctx context.Context, w config.Workflow, ch mattermost.Channel, p *delivery.Publisher, posts []mattermost.Post) ([]mattermost.Post, error) {
	handled := map[string]bool{}
	for _, post := range p.Posts {
		var id string
		if post.UserID == e.Bot.ID && post.ChannelID == ch.ID && post.Root() == p.Key.Root && post.DeleteAt == 0 && json.Unmarshal(post.Props["orpheus_model_command"], &id) == nil {
			handled[id] = true
		}
	}
	var commands []string
	for _, post := range posts {
		args, ok := modelArgs(post, ch, e.Bot)
		if !ok {
			continue
		}
		commands = append(commands, post.ID)
		if handled[post.ID] || post.CreateAt < w.Since.UnixMilli() {
			continue
		}
		store, ok := e.MM.(chatModels)
		if !ok {
			return nil, errors.New("chat preferences unavailable")
		}
		reply, err := e.modelReply(ctx, store, w, ch, post, args)
		if err != nil {
			return nil, err
		}
		_, err = e.MM.Create(ctx, mattermost.CreatePost{ChannelID: ch.ID, RootID: p.Key.Root, Message: reply, PendingID: "model-" + post.ID, Props: map[string]any{"orpheus_model_command": post.ID}})
		if err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(posts, func(post mattermost.Post) bool {
		return slices.Contains(commands, post.ID) || (post.UserID == e.Bot.ID && post.Props["orpheus_model_command"] != nil)
	}), nil
}
