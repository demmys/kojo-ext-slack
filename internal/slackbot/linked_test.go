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
		case strings.HasSuffix(r.URL.Path, "conversations.info"):
			switch r.Form.Get("channel") {
			case "CNO":
				_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
			case "CPRIV":
				_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"CPRIV","name":"secret","is_private":true}}`))
			case "CPRIV2":
				_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"CPRIV2","name":"team","is_private":true}}`))
			default:
				_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"` + r.Form.Get("channel") + `","name":"pub"}}`))
			}
		case strings.HasSuffix(r.URL.Path, "conversations.members"):
			if r.Form.Get("channel") == "CPRIV2" {
				_, _ = w.Write([]byte(`{"ok":true,"members":["UX","UREQ"]}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true,"members":["UX"]}`))
			}
		case strings.HasSuffix(r.URL.Path, "conversations.history") && r.Form.Get("channel") == "CPRIV2":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"UX","text":"team msg","ts":"1790000000.000500"}]}`))
		case strings.HasSuffix(r.URL.Path, "conversations.history") && r.Form.Get("channel") == "CPRIV":
			t.Errorf("private channel fetched for non-member")
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
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
	got := bot.resolveLinkedMessages(context.Background(), text, existing, "UREQ", "D1")
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

func TestResolveLinkedMessagesPrivateAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		ch := r.Form.Get("channel")
		switch {
		case strings.HasSuffix(r.URL.Path, "conversations.info"):
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"` + ch + `","name":"n` + ch + `","is_private":true}}`))
		case strings.HasSuffix(r.URL.Path, "conversations.members"):
			if ch == "CIN" {
				_, _ = w.Write([]byte(`{"ok":true,"members":["UX","UREQ"]}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true,"members":["UX"]}`))
			}
		case strings.HasSuffix(r.URL.Path, "conversations.history"):
			if ch != "CIN" {
				t.Errorf("fetched %s for non-member", ch)
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"UX","text":"ok","ts":"1790000000.000100"}]}`))
		case strings.HasSuffix(r.URL.Path, "conversations.replies"):
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
		}
	}))
	defer srv.Close()
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	got := bot.resolveLinkedMessages(context.Background(),
		"https://x.slack.com/archives/CIN/p1790000000000100 https://x.slack.com/archives/COUT/p1790000000000100",
		nil, "UREQ", "D1")
	if len(got) != 2 || got[0].Text != "ok" || got[0].ChannelName != "nCIN" || got[1].FetchError != errRequesterNotMember || got[1].Text != "" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(appendForwardedMessages("", got[1:]), "withheld") {
		t.Fatal("withheld note missing")
	}
}

func TestFetchLinkedReplyWithoutThreadTS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "conversations.replies") {
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"parent","ts":"1700000000.000001"},{"type":"message","user":"U2","text":"the reply","ts":"1790000000.000100"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	}))
	defer srv.Close()
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	fm := bot.fetchLinkedMessage(context.Background(), slackMessageRef{ChannelID: "C1", TS: "1790000000.000100"})
	if fm.Text != "the reply" || fm.FetchError != "" {
		t.Fatalf("fm = %+v", fm)
	}
}
