package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeMetadataContract(t *testing.T) {
	e := Envelope{Schema: 1, Source: "chat", Workflow: "assistant", Channel: "channel", Root: "root", Anchor: "anchor", Kind: "initial", TriggerIDs: []string{"anchor"}, Render: Render{Version: 2, MaxChars: 12000}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil || got.Anchor != e.Anchor || got.Render != e.Render {
		t.Fatalf("metadata lost: %+v %v", got, err)
	}
	for version := -1; version <= renderVersion+1; version++ {
		e.Render.Version = version
		_, err := Decode(json.RawMessage(mustJSON(e)))
		valid := version >= 1 && version <= renderVersion
		if (err == nil) != valid {
			t.Fatalf("render version %d: valid=%v err=%v", version, valid, err)
		}
	}
	for _, raw := range []json.RawMessage{nil, []byte("null"), []byte("[]"), []byte(`{"schema":1}`)} {
		if _, err := Decode(raw); err == nil {
			t.Fatalf("invalid metadata accepted: %s", raw)
		}
	}
}

func TestLocalFilesContractRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		e := Envelope{Schema: 1, Anchor: "anchor", Kind: "initial", TriggerIDs: []string{"anchor"}, Render: Render{Version: 4, MaxChars: 64}, LocalFiles: enabled}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"local_files"`) != enabled {
			t.Fatal("disabled flag changed the existing metadata contract")
		}
		got, err := Decode(raw)
		if err != nil || got.LocalFiles != enabled {
			t.Fatal("local file intent lost", got.LocalFiles, err)
		}
	}
}
