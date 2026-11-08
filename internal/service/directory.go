package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

// The transport observes the membership. The trusted account directory checks
// the expected human mapping and agent grants without returning any card data.
func confirmAccountCards(ctx context.Context, access *config.Access, roster []string) error {
	if !access.VerifyAccountCards {
		return nil
	}
	denied := errors.New("account directory did not confirm this audience")
	agent, ok := access.ScopeContext["agent_id"].(string)
	if !ok || agent == "" {
		return denied
	}
	accounts := map[string]config.SubjectID{}
	for _, id := range roster {
		if id == access.BotID {
			continue
		}
		if access.Peer != nil && id == access.Peer.ID {
			continue
		}
		mapped, ok := access.IdentityProjection.Subjects[id]
		if !ok {
			return denied
		}
		accounts[id] = mapped
	}
	if len(accounts) == 0 {
		return denied
	}
	address, err := url.Parse(os.Getenv("ORPHEUS_ACCOUNT_DIRECTORY_URL"))
	token := os.Getenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN")
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.Fragment != "" || address.RawQuery != "" || len(token) < 32 || strings.ContainsAny(token, "\r\n") {
		return denied
	}
	id := uuid.NewString()
	data, err := json.Marshal(map[string]any{"request_id": id, "method": "external.confirm_audience",
		"params": map[string]any{"provider": "mattermost", "agent_key": agent, "accounts": accounts}})
	if err != nil {
		return denied
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address.String(), bytes.NewReader(data))
	if err != nil {
		return denied
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := accountDirectoryHTTP.Do(req)
	if err != nil {
		return denied
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		return denied
	}
	if response.StatusCode != http.StatusOK {
		return denied
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	var result struct {
		ID        string `json:"request_id"`
		OK        bool   `json:"ok"`
		Confirmed bool   `json:"result"`
	}
	if err != nil || len(body) > 4096 || json.Unmarshal(body, &result) != nil || result.ID != id || !result.OK || !result.Confirmed {
		return denied
	}
	return nil
}

var accountDirectoryHTTP = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}()
