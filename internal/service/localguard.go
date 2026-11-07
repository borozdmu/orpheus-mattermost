package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

// localGate gives the host a veto over each dispatch and publication. It never
// issues credentials to a sandbox or forwards a message body to the policy service.
func localGate(ctx context.Context, w config.Workflow, env conversation.Envelope, audience []string, authors []string) error {
	if !w.Access.LocalPolicy() {
		return nil
	}
	b, err := json.Marshal(map[string]any{"workflow": w.ID, "channel": env.Channel, "bot": w.Access.BotID, "audience": audience, "authors": authors})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/admit", bytes.NewReader(b))
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/run/orpheus-policy/admission.sock")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("channel policy unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("channel policy denied")
	}
	return nil
}
