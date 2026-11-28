package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

const witnessTestSecret = "synthetic-test-secret-for-fresh-audience"

func signedAudienceExample(t *testing.T) agentcontract.Assignment {
	t.Helper()
	t.Setenv("ORPHEUS_AUDIENCE_SECRET", witnessTestSecret)
	author := agentcontract.Principal{Namespace: "example", ID: "private-test-author", PrincipalID: "42"}
	a := agentcontract.Assignment{
		Schema: agentcontract.Version, InvocationID: uuid.New(), Agent: "example", Author: author,
		Readers: []agentcontract.Principal{author}, Text: "private-test-assignment",
		Audience:     agentcontract.Audience{Kind: "exact", Instance: "example.invalid", ChannelID: "private-test-chat", ChannelType: "P", Complete: true},
		Delivery:     agentcontract.Route{Connector: "mattermost", Source: "example", Chat: "private-test-chat", Thread: "private-test-thread", Message: "private-test-message"},
		Confirmation: agentcontract.Confirmation{Revision: "synthetic-r1", Epoch: 1, ObservedAt: time.Now().UTC()},
	}
	if err := a.Seal(witnessTestSecret); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAudienceConfirmationBeyondServerWriteTimeout(t *testing.T) {
	a := signedAudienceExample(t)
	slowCheck := func(ctx context.Context, _ agentcontract.Assignment) error {
		select {
		case <-time.After(11 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var log bytes.Buffer
	protected := audienceConfirmationHandler(slog.New(slog.NewJSONHandler(&log, nil)), slowCheck)
	baseline := agentcontract.Handler(func() string { return witnessTestSecret }, slowCheck)
	type result struct {
		control bool
		witness agentcontract.Witness
		err     error
	}
	results := make(chan result, 2)
	var servers []*httptest.Server
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	for control, handler := range map[bool]http.Handler{false: protected, true: baseline} {
		server := httptest.NewUnstartedServer(handler)
		server.Config.WriteTimeout = 10 * time.Second
		server.Start()
		servers = append(servers, server)
		t.Cleanup(server.Close)
		go func() {
			witness, err := agentcontract.Confirm(ctx, server.Client(), server.URL, witnessTestSecret, a)
			results <- result{control, witness, err}
		}()
	}
	for range 2 {
		select {
		case r := <-results:
			if r.control {
				if r.err == nil || r.witness != (agentcontract.Witness{}) {
					t.Fatal("the unmodified ten-second listener returned a late witness")
				}
			} else if r.err != nil || r.witness.InvocationID != a.InvocationID.String() || r.witness.Digest != a.Confirmation.Digest || r.witness.ObservedAt.IsZero() {
				t.Fatalf("a verified delayed membership check did not produce the expected witness: %v", r.err)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the two independent HTTP exchanges")
		}
	}
	for _, server := range servers {
		server.Close()
	}
	if !strings.Contains(log.String(), `"result":"confirmed"`) {
		t.Fatal("slow accepted lookup was not recorded")
	}
}

func TestAudienceConfirmationRechecksAssignmentAtEndOfLookup(t *testing.T) {
	a := signedAudienceExample(t)
	var log bytes.Buffer
	var calls atomic.Int32
	server := httptest.NewServer(audienceConfirmationHandler(slog.New(slog.NewJSONHandler(&log, nil)), func(context.Context, agentcontract.Assignment) error {
		calls.Add(1)
		t.Setenv("ORPHEUS_AUDIENCE_SECRET", strings.Repeat("r", 40))
		return nil
	}))
	w, err := agentcontract.Confirm(t.Context(), server.Client(), server.URL, witnessTestSecret, a)
	server.Close()
	if err == nil || w != (agentcontract.Witness{}) || calls.Load() != 1 ||
		!strings.Contains(log.String(), `"result":"assignment_unconfirmed"`) {
		t.Fatal("a previously signed assignment survived key rotation during its membership check")
	}
}

func TestAudienceConfirmationDeniesInvalidOrRevokedAssignment(t *testing.T) {
	a := signedAudienceExample(t)
	var calls atomic.Int32
	var log bytes.Buffer
	server := httptest.NewServer(audienceConfirmationHandler(slog.New(slog.NewJSONHandler(&log, nil)), func(context.Context, agentcontract.Assignment) error {
		calls.Add(1)
		return errors.New("private-test-directory-detail")
	}))
	for _, tc := range []struct {
		name      string
		secret    string
		tamper    bool
		wantCalls int32
	}{
		{"bad bearer", strings.Repeat("x", 35), false, 0},
		{"altered assignment", witnessTestSecret, true, 0},
		{"revoked permission", witnessTestSecret, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			test := a
			if tc.tamper {
				test.Text += " altered after signing"
			}
			w, err := agentcontract.Confirm(t.Context(), server.Client(), server.URL, tc.secret, test)
			if err == nil || w != (agentcontract.Witness{}) || calls.Load() != tc.wantCalls {
				t.Fatal("a rejected assignment reached an unexpected admission or lookup")
			}
		})
	}
	server.Close()
	for _, private := range []string{witnessTestSecret, a.Author.ID, a.Text, a.Delivery.Chat, a.Delivery.Thread, a.Delivery.Message, a.Confirmation.Receipt, "private-test-directory-detail"} {
		if strings.Contains(log.String(), private) {
			t.Fatal("the operational log contains private assignment or directory details")
		}
	}
	if !strings.Contains(log.String(), `"result":"audience_unconfirmed"`) {
		t.Fatal("revoked permission was not recorded as unconfirmed")
	}
}

func TestAudienceConfirmationClientCancellationStopsLookup(t *testing.T) {
	a := signedAudienceExample(t)
	var log bytes.Buffer
	entered := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(audienceConfirmationHandler(slog.New(slog.NewJSONHandler(&log, nil)), func(ctx context.Context, _ agentcontract.Assignment) error {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil // A directory response can arrive as the caller cancels.
	}))
	t.Cleanup(server.Close)
	timeout, finish := context.WithTimeout(t.Context(), 5*time.Second)
	defer finish()
	ctx, cancel := context.WithCancel(timeout)
	defer cancel()
	out := make(chan error, 1)
	go func() {
		_, err := agentcontract.Confirm(ctx, server.Client(), server.URL, witnessTestSecret, a)
		out <- err
	}()
	select {
	case <-entered:
		cancel()
	case <-timeout.Done():
		t.Fatal("the membership lookup did not start")
	}
	select {
	case err := <-out:
		if err == nil {
			t.Fatal("the client accepted a cancelled lookup")
		}
	case <-timeout.Done():
		t.Fatal("the client ignored cancellation")
	}
	select {
	case <-finished:
	case <-timeout.Done():
		t.Fatal("the remote lookup failed to release its context")
	}
	server.Close()
	if !strings.Contains(log.String(), `"result":"lookup_cancelled"`) {
		t.Fatal("a cancelled lookup was not recorded")
	}
}
