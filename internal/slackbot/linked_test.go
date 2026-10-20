package slackbot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

func TestSlackMessageRefs(t *testing.T) {
	text := "see <https://x.slack.com/archives/C71J6AZ3J/p1790049629713879?thread_ts=1789992619.208539&amp;cid=C71J6AZ3J> and https://x.slack.com/archives/C2/p1790000000000100 dup https://x.slack.com/archives/C2/p1790000000000100"
	refs := slackMessageRefs(text)
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].ChannelID != "C71J6AZ3J" || refs[0].TS != "1790049629.713879" || refs[0].ThreadTS != "1789992619.208539" {
		t.Fatalf("ref0 = %+v", refs[0])
	}
	if refs[1].ChannelID != "C2" || refs[1].TS != "1790000000.000100" || refs[1].ThreadTS != "" {
		t.Fatalf("ref1 = %+v", refs[1])
	}
}

func TestResolveLinkedMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "conversations.history") && r.Form.Get("channel") == "COK":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U5","text":"hello","ts":"1790000000.000100","files":[{"id":"F7","name":"a.png"}]}]}`))
		case strings.HasSuffix(r.URL.Path, "conversations.replies"):
			if r.Form.Get("ts") != "1789992619.208539" {
				t.Errorf("replies ts = %q", r.Form.Get("ts"))
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U6","text":"reply","ts":"1790049629.713879"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
		}
	}))
	defer srv.Close()

	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	text := "https://x.slack.com/archives/COK/p1790000000000100 " +
		"https://x.slack.com/archives/CTH/p1790049629713879?thread_ts=1789992619.208539 " +
		"https://x.slack.com/archives/CNO/p1790000000000200 " +
		"https://x.slack.com/archives/CFWD/p1790000000000300"
	existing := []forwardedMessage{{IsShare: true, ChannelID: "CFWD", TS: "1790000000.000300"}}
	got := bot.resolveLinkedMessages(context.Background(), text, existing)
	if len(got) != 3 {
		t.Fatalf("got %d linked: %+v", len(got), got)
	}
	if got[0].AuthorID != "U5" || got[0].Text != "hello" || len(got[0].Files) != 1 || !got[0].Linked {
		t.Fatalf("ok link = %+v", got[0])
	}
	if got[1].Text != "reply" {
		t.Fatalf("thread link = %+v", got[1])
	}
	if got[2].FetchError != "not_in_channel" {
		t.Fatalf("failed link = %+v", got[2])
	}
	out := appendForwardedMessages("", got)
	for _, want := range []string{"[Linked message]", "text:\nhello\n", "files: a.png", "status: could not fetch (not_in_channel)", "[/Linked message]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "[Forwarded message]") {
		t.Errorf("linked rendered as forward:\n%s", out)
	}
}

func TestMsgUnfurlIsLinkedNotForward(t *testing.T) {
	msgs := forwardedMessagesFromPayload([]byte(`{"event":{"attachments":[{"is_msg_unfurl":true,"channel_id":"C1","ts":"1.000001","text":"x"},{"is_share":true,"text":"y"}]}}`))
	if len(msgs) != 2 || !msgs[0].Linked || msgs[1].Linked {
		t.Fatalf("msgs = %+v", msgs)
	}
}
