package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
)

const directoryProbeHeader = "X-Kord-Route-Probe"

var directoryProbeID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type directoryProbeKey struct{}

// Carry only a correlation ID through the ordinary signed-assignment handler.
func withDirectoryProbe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Values(directoryProbeHeader); len(h) == 1 && directoryProbeID.MatchString(h[0]) {
			r = r.WithContext(context.WithValue(r.Context(), directoryProbeKey{}, h[0]))
		}
		next.ServeHTTP(w, r)
	})
}

func directoryEvent(trace, id, stage, decision string) {
	if trace == "" {
		return
	}
	event := map[string]string{"kind": "kord.route.v1", "trace": trace, "request_id": id, "stage": stage}
	if decision != "" {
		event["decision"] = decision
	}
	_ = json.NewEncoder(os.Stdout).Encode(event)
}

// A missing result, duplicate field, alternate spelling, or null never proves refusal.
func directoryDecision(body []byte, id string) (bool, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return false, false
	}
	seen := map[string]bool{}
	result := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false, false
		}
		field, ok := key.(string)
		if !ok || seen[field] {
			return false, false
		}
		seen[field] = true
		value, err := dec.Token()
		if err != nil {
			return false, false
		}
		switch field {
		case "request_id":
			if value != id {
				return false, false
			}
		case "ok":
			if value != true {
				return false, false
			}
		case "result":
			b, ok := value.(bool)
			if !ok {
				return false, false
			}
			result = b
		default:
			return false, false
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 3 {
		return false, false
	}
	_, err = dec.Token()
	return result, err == io.EOF
}
