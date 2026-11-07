package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
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
		return &audienceDenial{code: "channel_policy_unavailable"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return policyReason(response.StatusCode, response.Body, audience, w.Access)
	}
	return nil
}

// Accept only the documented refusal codes and observed source identities; provider text is never shown.
func policyReason(status int, bodyReader io.Reader, audience []string, binding *config.Access) error {
	fallback := "channel_policy_denied"
	if status >= 500 {
		fallback = "channel_policy_unavailable"
	}
	if status != 403 && status != 503 {
		return &audienceDenial{code: fallback}
	}
	const maximum = 128 << 10
	body, err := io.ReadAll(io.LimitReader(bodyReader, maximum+1))
	var reason struct {
		Code string   `json:"code"`
		IDs  []string `json:"participant_ids"`
	}
	if err != nil || len(body) > maximum || json.Unmarshal(body, &reason) != nil {
		return &audienceDenial{code: fallback}
	}
	switch reason.Code {
	case "channel_policy_denied", "channel_policy_unavailable":
		return &audienceDenial{code: reason.Code}
	case "identity_unconfirmed", "identity_unmapped", "participant_access_unconfirmed":
		if len(reason.IDs) == 0 || len(reason.IDs) > len(audience) {
			return &audienceDenial{code: fallback}
		}
		ids := slices.Clone(reason.IDs)
		slices.Sort(ids)
		for i, id := range ids {
			if !config.ValidID(id) || !slices.Contains(audience, id) || id == binding.BotID || binding.Peer != nil && id == binding.Peer.ID || i > 0 && id == ids[i-1] {
				return &audienceDenial{code: fallback}
			}
		}
		return &audienceDenial{code: reason.Code, ids: ids}
	}
	return &audienceDenial{code: fallback}
}
