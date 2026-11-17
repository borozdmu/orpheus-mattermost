// A separately operated registrar publishes one short-lived approved occurrence
// per schedule to a root-owned read-only directory. The connector never accepts
// an owner, channel, prompt, or schedule definition from the calling service.
// The protocol is the same as in orpheus-telegram.
package service

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

var errScheduleUnconfirmed = errors.New("schedule unconfirmed")

// scheduleRegistryDir is a variable only for tests.
var scheduleRegistryDir = "/run/approved-schedule-occurrences"

type occurrenceRegistry struct {
	path  string
	token string
}

func registryFile(dir, name string) ([]byte, error) {
	d, err := os.Lstat(dir)
	if err != nil || !d.IsDir() || d.Sys().(*syscall.Stat_t).Uid != 0 || d.Mode().Perm()&0022 != 0 {
		return nil, errScheduleUnconfirmed
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errScheduleUnconfirmed
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0027 != 0 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errScheduleUnconfirmed
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(b) > 8192 {
		return nil, errScheduleUnconfirmed
	}
	return b, nil
}

func (v occurrenceRegistry) approved(r *http.Request) (registeredOccurrence, error) {
	var empty registeredOccurrence
	if r.Method != http.MethodPost || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+v.token)) != 1 || r.URL.RawQuery != "" || r.ContentLength != 0 {
		return empty, errScheduleUnconfirmed
	}
	s, err := uuid.Parse(r.PathValue("schedule"))
	if err != nil || s == uuid.Nil || s.String() != r.PathValue("schedule") {
		return empty, errScheduleUnconfirmed
	}
	o, err := uuid.Parse(r.PathValue("occurrence"))
	if err != nil || o == uuid.Nil || o.String() != r.PathValue("occurrence") {
		return empty, errScheduleUnconfirmed
	}
	raw, err := registryFile(v.path, s.String()+".json")
	if err != nil {
		return empty, errScheduleUnconfirmed
	}
	var p registeredOccurrence
	if json.Unmarshal(raw, &p) != nil || p.Schedule != s || p.Occurrence != o || o == s {
		return empty, errScheduleUnconfirmed
	}
	return p, nil
}

// scheduleHandler is nil when the operator did not mount the registry.
func scheduleHandler(lookup func(string) *Engine) (http.Handler, error) {
	if _, err := os.Lstat(scheduleRegistryDir); os.IsNotExist(err) {
		return nil, nil
	}
	b, err := registryFile(scheduleRegistryDir, "caller.key")
	if err != nil {
		return nil, err
	}
	token := strings.TrimSuffix(string(b), "\n")
	if len(token) < 32 || len(token) > 128 || strings.ContainsAny(token, " \r\n\t") {
		return nil, errScheduleUnconfirmed
	}
	return privateScheduleHandler(occurrenceRegistry{scheduleRegistryDir, token}.approved, lookup), nil
}

// A scheduled confirmation also expires when its registration is removed or
// replaced. An ordinary Mattermost assignment needs no schedule registration.
func currentSchedule(a agentcontract.Assignment, source string, now time.Time) bool {
	const prefix = "space-occurrence:"
	if !strings.HasPrefix(a.Delivery.Message, prefix) {
		return !strings.HasPrefix(a.Delivery.Thread, "space-schedule:")
	}
	s, err := uuid.Parse(strings.TrimPrefix(a.Delivery.Thread, "space-schedule:"))
	if err != nil || a.Delivery.Thread != "space-schedule:"+s.String() {
		return false
	}
	o, err := uuid.Parse(strings.TrimPrefix(a.Delivery.Message, prefix))
	if err != nil || a.Delivery.Message != prefix+o.String() || a.InvocationID != o {
		return false
	}
	raw, err := registryFile(scheduleRegistryDir, s.String()+".json")
	if err != nil {
		return false
	}
	var p registeredOccurrence
	return json.Unmarshal(raw, &p) == nil && p.Schedule == s && p.Occurrence == o && p.Source == source && p.Chat == a.Delivery.Chat && p.Agent == a.Agent && p.Actor == a.Author.ID && p.Principal == a.Author.PrincipalID && p.Revision == a.Confirmation.Revision && p.Text == a.Text && !now.Before(p.Slot) && now.Before(p.Slot.Add(5*time.Minute))
}

// Core confirms a scheduled assignment without a probe header. Derive the
// correlation ID from the approved occurrence, so the host observer can match
// the content-free route events with the same Space job. An explicit probe
// header keeps priority. Ordinary assignments get no trace.
func occurrenceTrace(o uuid.UUID) string {
	id := uuid.NewSHA1(o, []byte("audience/confirm"))
	return hex.EncodeToString(id[:])
}

func scheduleTrace(ctx context.Context, thread, message string) context.Context {
	if trace, _ := ctx.Value(directoryProbeKey{}).(string); trace != "" {
		return ctx
	}
	const prefix = "space-occurrence:"
	if !strings.HasPrefix(thread, "space-schedule:") || !strings.HasPrefix(message, prefix) {
		return ctx
	}
	o, err := uuid.Parse(strings.TrimPrefix(message, prefix))
	if err != nil || o == uuid.Nil || message != prefix+o.String() {
		return ctx
	}
	return context.WithValue(ctx, directoryProbeKey{}, occurrenceTrace(o))
}
