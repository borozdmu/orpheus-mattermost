package config

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Transcription contains process settings only; credentials are loaded at serve.
type Transcription struct {
	URL     string
	Model   string
	Timeout time.Duration
}

func loadTranscription(getenv func(string) string) (Transcription, error) {
	c := Transcription{URL: getenv("TRANSCRIPTION_URL"), Model: getenv("TRANSCRIPTION_MODEL"), Timeout: 2 * time.Minute}
	timeout := getenv("TRANSCRIPTION_TIMEOUT")
	if c.URL == "" && c.Model == "" && timeout == "" {
		return c, nil
	}
	if !validURL(c.URL) {
		return c, errors.New("invalid TRANSCRIPTION_URL")
	}
	normalized := strings.TrimSpace(c.URL)
	var err error
	u, _ := url.Parse(normalized)
	if u.Scheme != "https" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return c, errors.New("TRANSCRIPTION_URL requires HTTPS or loopback")
	}
	if c.Model == "" || len(c.Model) > 200 || strings.ContainsAny(c.Model, "\r\n\x00") {
		return c, errors.New("invalid TRANSCRIPTION_MODEL")
	}
	c.URL = normalized
	if timeout != "" {
		c.Timeout, err = time.ParseDuration(timeout)
		if err != nil || c.Timeout < time.Second || c.Timeout > 5*time.Minute {
			return c, errors.New("TRANSCRIPTION_TIMEOUT must be between 1s and 5m")
		}
	}
	return c, nil
}
