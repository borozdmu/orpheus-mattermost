package service

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus/agentcontract"
)

// Core allows 60 seconds for a current witness. Keep the roster read and the
// response inside that limit, independently of the listener's 10-second default.
const audienceReadBudget = 50 * time.Second

func audienceConfirmationHandler(logger *slog.Logger, confirm func(context.Context, agentcontract.Assignment) error) http.Handler {
	return withDirectoryProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), audienceReadBudget)
		defer cancel()
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(w)
		reason := "request_unconfirmed"
		var invocation string
		defer func() {
			elapsed := time.Since(start)
			if logger != nil && (reason != "confirmed" || elapsed >= 5*time.Second) {
				logger.Info("audience confirmation", "result", reason, "duration_ms", elapsed.Milliseconds(), "invocation_id", invocation)
			}
		}()
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			reason = "deadline_unavailable"
			http.Error(w, "audience unconfirmed", http.StatusServiceUnavailable)
			return
		}
		if err := controller.SetWriteDeadline(deadline.Add(3 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			reason = "deadline_unavailable"
			http.Error(w, "audience unconfirmed", http.StatusServiceUnavailable)
			return
		}
		// The shared handler authenticates the request, checks the assignment
		// signature and age, and only then invokes this live membership check.
		handler := agentcontract.Handler(func() string { return os.Getenv("ORPHEUS_AUDIENCE_SECRET") }, func(ctx context.Context, a agentcontract.Assignment) error {
			invocation = a.InvocationID.String()
			err := confirm(ctx, a)
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if err == nil {
				if err := a.Verify(os.Getenv("ORPHEUS_AUDIENCE_SECRET"), time.Now()); err != nil {
					reason = "assignment_unconfirmed"
					return err
				}
			}
			reason = audienceCheckResult(err)
			return err
		})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func audienceCheckResult(err error) string {
	if err == nil {
		return "confirmed"
	}
	if errors.Is(err, context.Canceled) {
		return "lookup_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "lookup_timeout"
	}
	if failure, ok := errors.AsType[net.Error](err); ok {
		if failure.Timeout() {
			return "lookup_timeout"
		}
		return "lookup_unavailable"
	}
	if status := mattermost.Status(err); status == http.StatusTooManyRequests || status >= 500 {
		return "lookup_unavailable"
	}
	if denial, ok := errors.AsType[*audienceDenial](err); ok && denial.code == channelUnavailable {
		return "roster_unconfirmed"
	}
	return "audience_unconfirmed"
}
