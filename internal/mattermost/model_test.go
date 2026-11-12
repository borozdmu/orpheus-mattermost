package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFirstChatModelPreference(t *testing.T) {
	const bot = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	const channel = "cccccccccccccccccccccccccc"
	const command = "pppppppppppppppppppppppppp"
	var stored []modelPreference
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v4/users/"+bot+"/preferences/model_"+channel:
			if len(stored) == 0 {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
		case r.Method == "PUT" && r.URL.Path == "/api/v4/users/"+bot+"/preferences":
			if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "test", time.Second)
	settings, err := c.ModelSettings(t.Context(), bot, channel)
	if err != nil || len(settings) != 0 {
		t.Fatal(settings, err)
	}
	choice := ModelSetting{Profile: "base--low", Base: "base", At: 100, Reply: "ok"}
	if err := c.SaveModelSetting(t.Context(), bot, channel, command, choice); err != nil {
		t.Fatal(err)
	}
	settings, err = c.ModelSettings(t.Context(), bot, channel)
	if err != nil || settings[command] != choice {
		t.Fatal(settings, err)
	}
}
