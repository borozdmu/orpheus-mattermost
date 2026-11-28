package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
	"github.com/orpheus-agents/orpheus-mattermost/internal/service"
)

func checkAcceptedFile(args []string) error {
	flags := flag.NewFlagSet("check-accepted-file", flag.ContinueOnError)
	workflow := flags.String("workflow", "", "accepted workflow")
	channel := flags.String("channel", "", "accepted channel")
	root := flags.String("root", "", "accepted root post")
	post := flags.String("post", "", "accepted anchor post")
	file := flags.String("file", "", "accepted attachment")
	run := flags.String("run", "", "accepted run")
	sha := flags.String("sha256", "", "independently known file digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	w := cfg.StateWorkflow(*workflow, *channel)
	if w == nil || w.Access == nil {
		return errors.New("no protected workflow for selected channel")
	}
	coreToken, err := config.Secret(cfg.Orpheus.TokenEnv)
	if err != nil {
		return err
	}
	api, err := orpheus.New(cfg, coreToken)
	if err != nil {
		return err
	}
	credential, err := config.Secret(w.Mattermost.TokenEnv)
	if err != nil {
		return err
	}
	mm := mattermost.New(w.Mattermost.BaseURL, credential, cfg.HTTPTimeout.Value())
	e := service.Engine{Config: cfg, API: api, MM: mm}
	key := conversation.Key{Source: w.Mattermost.Source(w.Access.BotID), Workflow: w.ID, Channel: *channel, Root: *root}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	size, err := e.CheckAcceptedFile(ctx, *w, key, *run, *post, *file, *sha)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Bytes int64  `json:"bytes"`
		SHA   string `json:"sha256"`
	}{size, *sha})
}
