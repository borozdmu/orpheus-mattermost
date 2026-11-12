package service

import (
	"context"
	"testing"

	api "github.com/orpheus-agents/orpheus/client"

	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

func TestSettingsForNewThread(t *testing.T) {
	settings := map[string]mattermost.ModelSetting{"a": {At: 10, Base: "bot", Profile: "bot--fast"}, "b": {At: 20, Base: "bot", Profile: "bot--slow"}, "c": {At: 30, Base: "other", Profile: "other--default"}}
	for _, tc := range []struct {
		at   int64
		want string
	}{{9, "bot"}, {15, "bot--fast"}, {30, "bot--slow"}} {
		if got := selectedChatModel(settings, "bot", tc.at, "z"); got != tc.want {
			t.Fatalf("%d: %s", tc.at, got)
		}
	}
}
func TestModelCommandIsAddressed(t *testing.T) {
	ch := mattermost.Channel{ID: "c", Type: "O"}
	bot := mattermost.User{Username: "mark", ID: "b"}
	for _, tc := range []struct {
		text string
		want bool
	}{{"@mark /model", true}, {"@mark /model beta high", true}, {"@someone /model", false}, {"/model beta high", false}, {"please @mark /model", false}} {
		post := mattermost.Post{ChannelID: "c", UserID: "u", Message: tc.text}
		if _, ok := modelArgs(post, ch, bot); ok != tc.want {
			t.Fatal(tc.text)
		}
	}
}

type modelSource struct {
	*fakeSource
	admin    bool
	settings map[string]mattermost.ModelSetting
	writes   int
}

func (s *modelSource) ChatAdmin(context.Context, mattermost.Channel, mattermost.User) (bool, error) {
	return s.admin, nil
}
func (s *modelSource) ModelSettings(context.Context, string, string) (map[string]mattermost.ModelSetting, error) {
	return s.settings, nil
}
func (s *modelSource) SaveModelSetting(_ context.Context, _, _, post string, setting mattermost.ModelSetting) error {
	s.settings[post] = setting
	s.writes++
	return nil
}

type modelAPI struct {
	*fakeAPI
	base string
}

func (a modelAPI) Profiles(context.Context) ([]api.Profile, error) {
	return []api.Profile{{Name: a.base + "--fast", Model: new("fast"), Codex: api.CodexProfile{Effort: new(api.CodexProfileEffortLow)}}}, nil
}
func TestAdminCommandReceiptsAndNextThread(t *testing.T) {
	for _, admin := range []bool{false, true} {
		e, core, source, _, key := fixture(t)
		w := e.Config.Workflows[0]
		mm := &modelSource{fakeSource: source, admin: admin, settings: map[string]mattermost.ModelSetting{}}
		e.MM = mm
		e.API = modelAPI{core, w.Profile}
		ch := mattermost.Channel{ID: key.Channel, Type: "O"}
		root := source.posts[0]
		source.posts[0].Message = "@orpheus /model fast low"
		for range 2 {
			if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
				t.Fatal(err)
			}
		}
		if source.calls != 1 || len(core.submitted) != 0 || (mm.writes == 1) != admin {
			t.Fatal(source.calls, mm.writes, core.submitted)
		}
		key.Root = "00000000000000000000000033"
		root.ID = key.Root
		root.CreateAt++
		source.posts = []mattermost.Post{root}
		if err := e.Thread(t.Context(), w, key, ch, true); err != nil {
			t.Fatal(err)
		}
		want := w.Profile
		if admin {
			want += "--fast"
		}
		if len(core.submitted) != 1 || core.submitted[0].Workflow.Profile != want {
			t.Fatalf("wanted %q, received %+v", want, core.submitted)
		}
	}
}
