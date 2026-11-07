package transcription

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMultipartVoiceTranscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("wrong request identity")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		if r.FormValue("model") != "speech-model" || r.FormValue("response_format") != "json" {
			t.Error("wrong model/format")
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = file.Close() }()
		data, _ := io.ReadAll(file)
		if string(data) != "OggS-test" || header.Filename != "audio.input" || header.Header.Get("Content-Type") != "application/octet-stream" {
			t.Error("voice upload changed")
		}
		_, _ = io.WriteString(w, `{"text":"  Подготовь отчёт.  ","languages":[{"code":"ru"}]}`)
	}))
	defer server.Close()
	c := New(server.URL+"/v1/audio/transcriptions", "speech-model", "secret", time.Second)
	text, err := c.Transcribe(t.Context(), []byte("OggS-test"))
	if err != nil || text != "Подготовь отчёт." {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestRejectedResponsesNeverRetryOrExposeBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"unauthorized", 401, "secret audio text"},
		{"rate limited", 429, "secret audio text"},
		{"server error", 500, "secret audio text"},
		{"malformed", 200, "secret audio text"},
		{"empty", 200, `{"text":"  "}`},
		{"large text", 200, `{"text":"` + strings.Repeat("x", MaxTextBytes+1) + `"}`},
		{"large response", 200, strings.Repeat("x", (256<<10)+1)},
		{"nul", 200, `{"text":"bad\u0000text"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := New(server.URL, "model", "secret", time.Second).Transcribe(t.Context(), []byte("OggS"))
			if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestNoRedirectOrReplay(t *testing.T) {
	redirects := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects++ }))
	defer target.Close()
	for _, status := range []int{301, 302, 307, 308} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		_, err := New(server.URL, "model", "secret", time.Second).Transcribe(t.Context(), []byte("OggS"))
		server.Close()
		if err == nil || redirects != 0 {
			t.Fatal("followed transcription redirect")
		}
	}
}

func TestCancelledRequestAndInvalidAudio(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = io.WriteString(w, `{"text":"test"}`) }))
	defer server.Close()
	c := New(server.URL, "model", "secret", time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Transcribe(ctx, []byte("OggS")); err == nil {
		t.Fatal("cancelled request accepted")
	}
	for _, data := range [][]byte{nil, make([]byte, 20_000_001)} {
		if _, err := c.Transcribe(t.Context(), data); err == nil {
			t.Fatal("invalid audio accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached provider")
	}
}

func TestAudioRejectionIsDifferentFromCapacity(t *testing.T) {
	for _, status := range []int{400, 413, 415, 422, 401, 429, 503} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		_, err := New(s.URL, "model", "secret", time.Second).Transcribe(t.Context(), []byte("OggS-test"))
		s.Close()
		if errors.Is(err, ErrAudio) != (status == 400 || status == 413 || status == 415 || status == 422) {
			t.Fatalf("status=%d error=%v", status, err)
		}
	}
}
