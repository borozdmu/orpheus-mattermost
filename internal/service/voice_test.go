package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
 "github.com/orpheus-agents/orpheus-mattermost/internal/transcription"
)

const audioID = "vvvvvvvvvvvvvvvvvvvvvvvvvv"

type audioSource struct {
	*fakeSource
	denied, edited bool
	downloads      int
}

func (s *audioSource) Post(_ context.Context, id string) (mattermost.Post, error) {
	for _, p := range s.posts {
		if p.ID == id {
			if s.edited {
				p.Message += "edited"
			}
			return p, nil
		}
	}
	return mattermost.Post{}, errors.New("missing post")
}
func (s *audioSource) Files(_ context.Context, id string) ([]mattermost.FileInfo, error) {
	if id != s.posts[0].ID {
		return nil, nil
	}
	return []mattermost.FileInfo{{ID: audioID, PostID: id, Name: "voice.ogg", MIME: "audio/ogg", Size: 9}}, nil
}
func (s *audioSource) File(ctx context.Context, id string) (mattermost.FileInfo, error) {
	if s.denied || id != audioID {
		return mattermost.FileInfo{}, errors.New("denied")
	}
	files, _ := s.Files(ctx, s.posts[0].ID)
	return files[0], nil
}
func (s *audioSource) Download(context.Context, string, int64) ([]byte, error) {
	s.downloads++
	return []byte("OggS-test"), nil
}

type voiceFake struct {
	calls   int
	failure bool
	text    string
}

func (v *voiceFake) Transcribe(context.Context, []byte) (string, error) {
	v.calls++
	if v.failure {
		return "", errors.New("recognizer unavailable")
	}
	return v.text, nil
}

func voiceFixture(t *testing.T) (*Engine, *fakeAPI, *audioSource, *voiceFake, conversation.Key) {
	t.Helper()
	e, api, mm, _, key := fixture(t)
	mm.posts[0].FileIDs = []string{audioID}
	src := &audioSource{fakeSource: mm}
	voice := &voiceFake{text: "Подготовь отчёт за сентябрь."}
	e.MM = src
	e.Voice = voice
	e.Config.Workflows[0].TranscribeVoice = true
	return e, api, src, voice, key
}

func TestVoiceLostAdmissionDoesNotRepeatRecognitionOrSubmission(t *testing.T) {
	e, api, src, v, key := voiceFixture(t)
	api.lost = true
	w := e.Config.Workflows[0]
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	if err := e.Thread(t.Context(), w, key, channel, true); err == nil {
		t.Fatal("expected lost response")
	}
	if len(api.submitted) != 1 || v.calls != 1 || src.downloads != 1 {
		t.Fatal("missing recognition/admission")
	}
	p := api.submitted[0]
	if !strings.Contains(joinedInput(p.Messages), v.text) || len(p.Envelope.Request.Files) != 1 {
		t.Fatal("transcript or original attachment lost")
	}
	if err := e.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 || v.calls != 1 {
		t.Fatal("duplicate recognition/admission")
	}
}

func TestAudioFailureDoesNotSubmitPartialInput(t *testing.T) {
	for _, which := range []string{"denied", "edited", "recognizer"} {
		t.Run(which, func(t *testing.T) {
			e, api, src, v, key := voiceFixture(t)
			src.denied = which == "denied"
			src.edited = which == "edited"
			v.failure = which == "recognizer"
			if err := e.Thread(t.Context(), e.Config.Workflows[0], key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err == nil {
				t.Fatal("expected rejection")
			}
			if len(api.submitted) != 0 {
				t.Fatal("partial input admitted")
			}
			if which != "recognizer" && v.calls != 0 {
				t.Fatal("unverified audio reached recognizer")
			}
		})
	}
}

func TestVoiceProjectionPreservesOriginalOnBudgetFailure(t *testing.T) {
	e, _, src, v, key := voiceFixture(t)
	post := src.posts[0]
	p := pending{Workflow: e.Config.Workflows[0], Envelope: conversation.Envelope{Versions: []conversation.Version{conversation.PostVersion(post)}, Request: attachments.Request{Limits: e.Config.Workflows[0].Files, Files: []attachments.InputFile{{PostID: post.ID, FileID: audioID, ChannelID: key.Channel, MIME: "audio/ogg", Size: 9}}}}, Messages: []conversation.InputMessage{{PostID: post.ID, Text: "unchanged"}}}
	if err := projectVoiceInput(t.Context(), &p, src, v, 10); err == nil {
		t.Fatal("budget not enforced")
	}
	if p.Messages[0].Text != "unchanged" {
		t.Fatal("partial mutation escaped")
	}
}

func TestVoiceOptInLeavesExistingAttachmentsUnchanged(t *testing.T) {
	e, api, _, v, key := voiceFixture(t)
	w := e.Config.Workflows[0]
	w.TranscribeVoice = false
	if err := e.Thread(t.Context(), w, key, mattermost.Channel{ID: key.Channel, Type: "O"}, true); err != nil {
		t.Fatal(err)
	}
	if v.calls != 0 || len(api.submitted) != 1 || strings.Contains(joinedInput(api.submitted[0].Messages), "Расшифровка") {
		t.Fatal("voice was not opt-in")
	}
}

 type rejectedVoice struct{}
 func (rejectedVoice) Transcribe(context.Context, []byte) (string,error) { return "",transcription.ErrAudio }
 func TestUnsupportedAudioIsRejectedOnceWithoutRun(t *testing.T) {
  e,api,src,_,key := voiceFixture(t)
  e.Voice = rejectedVoice{}
  w := e.Config.Workflows[0]
  channel := mattermost.Channel{ID:key.Channel,Type:"O"}
  if err:=e.Thread(t.Context(),w,key,channel,true);err!=nil {t.Fatal(err)}
  if err:=e.Thread(t.Context(),w,key,channel,true);err!=nil {t.Fatal(err)}
  if len(api.submitted)!=0 || src.calls!=1 || src.downloads!=1 {t.Fatalf("submits=%d notices=%d downloads=%d",len(api.submitted),src.calls,src.downloads)}
 }
