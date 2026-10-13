package slackbot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/slack-go/slack"
)

type mockMgr struct{}

func (m *mockMgr) ChatOneShot(_ context.Context, _, _ string, _ agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, 1)
	ch <- agent.ChatEvent{Type: "done"}
	close(ch)
	return ch, nil
}

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// mockSlackServer returns an httptest.Server that handles common Slack API calls.
func mockSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprintf(w, `{"ok":true,"user_id":"UBOTTEST","user":"testbot","team":"TestTeam","team_id":"T1"}`)
		case "/users.info":
			_ = r.ParseForm()
			uid := r.FormValue("user")
			switch uid {
			case "U123":
				fmt.Fprintf(w, `{"ok":true,"user":{"id":"U123","name":"alice","profile":{"display_name":"Alice"}}}`)
			case "U456":
				fmt.Fprintf(w, `{"ok":true,"user":{"id":"U456","name":"bob","profile":{},"real_name":"Bob Real"}}`)
			default:
				fmt.Fprintf(w, `{"ok":false,"error":"user_not_found"}`)
			}
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- Bot lifecycle tests ---

// keyedRegistrarMgr records the keyed background registration Run makes.
type keyedRegistrarMgr struct {
	mockMgr
	registered   chan agent.KeyedBackgroundHandler
	unregistered chan struct{}
}

func (m *keyedRegistrarMgr) RegisterKeyedBackgroundHandler(_ string, h agent.KeyedBackgroundHandler) func() {
	m.registered <- h
	return func() { close(m.unregistered) }
}

func TestBotRunStop(t *testing.T) {
	srv := mockSlackServer(t)
	mgr := &keyedRegistrarMgr{
		registered:   make(chan agent.KeyedBackgroundHandler, 1),
		unregistered: make(chan struct{}),
	}
	bot := NewBot(context.Background(), "a1", t.TempDir(), agent.SlackBotConfig{Enabled: true},
		"xapp-test-123", "xoxb-test-456", mgr, testLogger, slack.OptionAPIURL(srv.URL+"/"))
	go bot.Run()

	select {
	case h := <-mgr.registered:
		if h != agent.KeyedBackgroundHandler(bot) {
			t.Fatal("Run registered a handler other than the bot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not register the keyed background handler")
	}
	if bot.botUserID != "UBOTTEST" {
		t.Fatalf("botUserID = %q, want UBOTTEST", bot.botUserID)
	}

	bot.Stop()
	select {
	case <-bot.Done():
	default:
		t.Fatal("Done is not closed after Stop")
	}
	select {
	case <-mgr.unregistered:
	default:
		t.Fatal("Stop did not unregister the keyed background handler")
	}
}

// A rejected token makes Run return instead of connecting, so the caller can
// tell a bot that never started from one that was stopped.
func TestBotRunReturnsOnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	t.Cleanup(srv.Close)
	mgr := &keyedRegistrarMgr{
		registered:   make(chan agent.KeyedBackgroundHandler, 1),
		unregistered: make(chan struct{}),
	}
	bot := NewBot(context.Background(), "a1", t.TempDir(), agent.SlackBotConfig{Enabled: true},
		"xapp-test-123", "xoxb-bad", mgr, testLogger, slack.OptionAPIURL(srv.URL+"/"))
	go bot.Run()

	select {
	case <-bot.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after auth.test failed")
	}
	select {
	case <-mgr.registered:
		t.Fatal("Run registered a background handler without a working token")
	default:
	}
	bot.Stop()
}
