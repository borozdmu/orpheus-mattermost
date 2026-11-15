package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

const directAdmissionMode = "MATTERMOST_CHANNEL_ADMISSION_TRANSPORT"
const admissionCredentialPath = "/run/orpheus-channel-admission/client.json"
const admissionSocket = "/run/kord-channel-data/api.sock"
const channelUnavailable = "channel_policy_unavailable"

type channelRequest struct {
	Workflow string   `json:"workflow"`
	Channel  string   `json:"channel"`
	Bot      string   `json:"bot"`
	Audience []string `json:"audience"`
	Authors  []string `json:"authors"`
}

type channelCredentials struct{ token, binding string }

func unavailable() error { return &audienceDenial{code: channelUnavailable} }

// exactFields preserves null and rejects duplicate, missing, and extension fields.
func exactFields(body []byte, keys ...string) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(body))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for d.More() {
		token, err := d.Token()
		k, ok := token.(string)
		if err != nil || !ok || !slices.Contains(keys, k) || fields[k] != nil {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		fields[k] = value
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || len(fields) != len(keys) {
		return nil, false
	}
	_, err = d.Token()
	return fields, err == io.EOF
}

func bounded(r io.Reader, limit int64) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	return b, err == nil && int64(len(b)) <= limit
}

func decodeCredentials(raw []byte) (channelCredentials, bool) {
	f, ok := exactFields(raw, "token", "binding")
	var c channelCredentials
	if !ok || json.Unmarshal(f["binding"], &c.binding) != nil || json.Unmarshal(f["token"], &c.token) != nil ||
		len(c.binding) != 64 || strings.Trim(c.binding, "0123456789abcdef") != "" || len(c.token) < 32 || len(c.token) > 512 {
		return channelCredentials{}, false
	}
	for i := range len(c.token) {
		if c.token[i] < 33 || c.token[i] > 126 {
			return channelCredentials{}, false
		}
	}
	return c, true
}

func readChannelCredentials() (channelCredentials, bool) {
	f, err := os.OpenFile(admissionCredentialPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return channelCredentials{}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0037 != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return channelCredentials{}, false
	}
	raw, ok := bounded(f, 1024)
	if !ok {
		return channelCredentials{}, false
	}
	return decodeCredentials(raw)
}

func unixAdmissionClient(path string) *http.Client {
	transport := &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "unix", path)
	}}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var channelHTTP = unixAdmissionClient(admissionSocket)

func directGate(ctx context.Context, params channelRequest, access *config.Access, c channelCredentials, client *http.Client) error {
	id := uuid.NewString()
	trace, _ := ctx.Value(directoryProbeKey{}).(string)
	raw, ok := callAdmission(ctx, client, c, id, "channel.admit", params, 8192, trace)
	if !ok {
		return unavailable()
	}
	result := channelDecision(raw, id, c.binding, access)
	if result == nil {
		directoryEvent(trace, id, "decision", "allow")
	} else if d, ok := errors.AsType[*audienceDenial](result); ok && d.code != channelUnavailable {
		directoryEvent(trace, id, "decision", "deny")
	}
	return result
}

func callAdmission(ctx context.Context, client *http.Client, c channelCredentials, id, method string, params any, limit int, trace string) ([]byte, bool) {

	body, err := json.Marshal(map[string]any{"method": method, "params": params, "request_id": id})
	if err != nil || len(body) > limit {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/internal/v1/call", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	if trace != "" {
		req.Header.Set(directoryProbeHeader, trace)
		directoryEvent(trace, id, "rpc", "")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]) != "application/json" {
		return nil, false
	}
	raw, ok := bounded(resp.Body, 8192)
	if !ok {
		return nil, false
	}
	return raw, true
}

func channelDecision(raw []byte, id, pin string, access *config.Access) error {
	envelope, ok := exactFields(raw, "ok", "request_id", "result")
	var rid, binding string
	if !ok || string(envelope["ok"]) != "true" || json.Unmarshal(envelope["request_id"], &rid) != nil || rid != id {
		return unavailable()
	}
	r, ok := exactFields(envelope["result"], "binding", "reason", "participants")
	var ids []string
	if !ok || json.Unmarshal(r["binding"], &binding) != nil || binding != pin || json.Unmarshal(r["participants"], &ids) != nil || ids == nil || !slices.IsSorted(ids) {
		return unavailable()
	}
	for i, p := range ids {
		if _, found := access.IdentityProjection.Subjects[p]; !found || !config.ValidID(p) || i > 0 && p == ids[i-1] {
			return unavailable()
		}
	}
	if string(r["reason"]) == "null" && len(ids) == 0 {
		return nil
	}
	var reason string
	if json.Unmarshal(r["reason"], &reason) != nil {
		return unavailable()
	}
	switch {
	case reason == "channel_policy_denied" && len(ids) == 0:
		return &audienceDenial{code: reason}
	case (reason == "participant_access_unconfirmed" || reason == "identity_unconfirmed") && len(ids) > 0:
		return &audienceDenial{code: reason, ids: ids}
	default:
		return unavailable()
	}
}
