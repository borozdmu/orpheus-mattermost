package config

import (
	"testing"
	"time"
)

func TestTranscriptionEndpointAndDeadline(t *testing.T) {
	valid := map[string]string{"TRANSCRIPTION_URL": "https://speech.example/v1/audio/transcriptions", "TRANSCRIPTION_MODEL": "gigaam-v3", "TRANSCRIPTION_TIMEOUT": "240s"}
	get := func(k string) string { return valid[k] }
	c, err := loadTranscription(get)
	if err != nil || c.Timeout != 4*time.Minute {
		t.Fatal(c, err)
	}
	for _, endpoint := range []string{"http://remote.example/v1", "https://secret@host/v1", "https://speech.example/v1?token=x", "https://speech.example/v1#x"} {
		valid["TRANSCRIPTION_URL"] = endpoint
		if _, err := loadTranscription(get); err == nil {
			t.Fatalf("accepted endpoint %s", endpoint)
		}
	}
	valid["TRANSCRIPTION_URL"] = "http://127.0.0.1:18080/v1"
	if _, err := loadTranscription(get); err != nil {
		t.Fatal(err)
	}
	valid["TRANSCRIPTION_TIMEOUT"] = "6m"
	if _, err := loadTranscription(get); err == nil {
		t.Fatal("unbounded timeout")
	}
}
