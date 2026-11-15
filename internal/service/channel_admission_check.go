package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

// ChannelAdmissionBindings identifies routes for private probes without exporting participant identities.
func ChannelAdmissionBindings(cfg config.Config) (map[string]string, error) {
	if os.Getenv(directAdmissionMode) != "data" {
		return nil, errors.New("direct channel admission is not configured")
	}
	routes := map[string]string{}
	for _, w := range cfg.StateWorkflows() {
		if !w.Access.LocalPolicy() || w.OrdinaryUse() {
			continue
		}
		if len(w.IncludeIDs) != 1 || !config.ValidID(w.IncludeIDs[0]) || routes[w.ID] != "" {
			return nil, errors.New("ambiguous channel admission binding")
		}
		if cfg.StateWorkflow(w.ID, w.IncludeIDs[0]) == nil {
			continue
		}
		routes[w.ID] = w.IncludeIDs[0]
	}
	if len(routes) == 0 {
		return nil, errors.New("no channel admission bindings")
	}
	return routes, nil
}

// CheckChannelAdmission only calls the ordinary admission client. It cannot start a run or post to Mattermost.
func CheckChannelAdmission(ctx context.Context, cfg config.Config, trace string, in io.Reader, out io.Writer) error {
	if !directoryProbeID.MatchString(trace) {
		return errors.New("invalid channel admission trace")
	}
	raw, ok := bounded(in, 8192)
	_, valid := exactFields(raw, "workflow", "channel", "bot", "audience", "authors")
	var p channelRequest
	if !ok || !valid || json.Unmarshal(raw, &p) != nil || p.Audience == nil || p.Authors == nil {
		return errors.New("invalid channel admission input")
	}
	routes, err := ChannelAdmissionBindings(cfg)
	if err != nil {
		return err
	}
	w := cfg.StateWorkflow(p.Workflow, p.Channel)
	if w == nil || !w.Access.LocalPolicy() || routes[p.Workflow] != p.Channel || w.Access.BotID != p.Bot {
		return errors.New("unregistered channel admission input")
	}
	ctx = context.WithValue(ctx, directoryProbeKey{}, trace)
	err = localGate(ctx, *w, conversation.Envelope{Channel: p.Channel}, p.Audience, p.Authors)
	status := http.StatusNoContent
	var code string
	if err != nil {
		status, code = http.StatusServiceUnavailable, channelUnavailable
		if d, yes := errors.AsType[*audienceDenial](err); yes && d.code != channelUnavailable {
			status, code = http.StatusForbidden, d.code
		}
	}
	return json.NewEncoder(out).Encode(struct {
		Status int    `json:"status"`
		Code   string `json:"code,omitempty"`
	}{status, code})
}
