package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

// resolveLiveHumans fills only missing identities for the current observation.
// Configured bindings stay immutable, including operator and bot identities.
func resolveLiveHumans(ctx context.Context, access config.Access, roster []string) (config.Access, error) {
	if os.Getenv("ORPHEUS_LIVE_AUDIENCE") == "false" || !access.VerifyAccountCards || access.LocalPolicy() {
		return access, nil
	}
	denied := errors.New("directory could not resolve current recipients")
	humans := make([]string, 0, len(roster))
	missing := false
	for _, id := range roster {
		if id == access.BotID || access.ReceiveOnly(id) {
			continue
		}
		if !config.ValidID(id) {
			return access, denied
		}
		humans = append(humans, id)
		if _, ok := access.IdentityProjection.Subjects[id]; !ok {
			missing = true
		}
	}
	if !missing {
		return access, nil
	}
	if len(humans) > 1000 {
		return access, denied
	}
	addr, err := accountDirectoryURL(sharedProviderDirectory)
	token := os.Getenv("ORPHEUS_ACCOUNT_DIRECTORY_TOKEN")
	if err != nil || len(token) < 32 || strings.ContainsAny(token, "\r\n") {
		return access, denied
	}
	reqID := uuid.NewString()
	params := map[string]any{"provider": "mattermost", "agent_key": access.ScopeContext["agent_id"], "external_ids": humans}
	body, err := json.Marshal(map[string]any{"method": "external.resolve_audience_accounts", "params": params, "request_id": reqID})
	if err != nil {
		return access, denied
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, bytes.NewReader(body))
	if err != nil {
		return access, denied
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := accountDirectoryHTTP.Do(req)
	if err != nil {
		return access, denied
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return access, denied
	}
	var response struct {
		RequestID string           `json:"request_id"`
		OK        bool             `json:"ok"`
		Result    map[string]int64 `json:"result"`
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &response) != nil || !response.OK || response.RequestID != reqID || len(response.Result) != len(humans) {
		return access, denied
	}
	subjects := maps.Clone(access.IdentityProjection.Subjects)
	for _, id := range humans {
		uid := response.Result[id]
		if uid <= 0 {
			return access, denied
		}
		value := strconv.FormatInt(uid, 10)
		if prev, ok := subjects[id]; ok && string(prev) != value {
			return access, denied
		}
		subjects[id] = config.SubjectID(value)
	}
	access.IdentityProjection.Subjects = subjects
	return access, access.Validate()
}
