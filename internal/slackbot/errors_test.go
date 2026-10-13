package slackbot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

func TestSlackChatErrorSanitizesAndBounds(t *testing.T) {
	input := "Bearer private-token Basic cHJpdmF0ZQ== password=\"multi word secret\" https://user:password@example.com token=private-query sk-privateKEY <!channel> ``` " + noReplyToken
	got := slackChatError(input)
	for _, secret := range []string{"private-token", "cHJpdmF0ZQ==", "multi word secret", "user:password", "private-query", "sk-privateKEY", "<!channel>", noReplyToken} {
		if strings.Contains(got, secret) {
			t.Fatalf("leaked %q: %s", secret, got)
		}
	}
	if strings.Count(got, "```") != 2 {
		t.Fatalf("markup injection: %s", got)
	}
	if len([]rune(slackChatError(strings.Repeat("あ", 4000)))) > 1300 {
		t.Fatal("unbounded diagnostic")
	}
	if !strings.Contains(slackChatError("this conversation already has a goal; clear it before starting another"), "!goal resume") {
		t.Fatal("missing recovery command")
	}
}

func TestSendToAgentShowsBackendError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		events    []agent.ChatEvent
		streaming bool
		partial   string
	}{
		{"tool-only failure", []agent.ChatEvent{{Type: "tool_use", ToolName: "Bash"}, {Type: "error", ErrorMessage: "specific failure"}}, true, ""},
		{"error only", []agent.ChatEvent{{Type: "error", ErrorMessage: "specific failure"}}, false, ""},
		{"done error", []agent.ChatEvent{{Type: "done", ErrorMessage: "specific failure"}}, false, ""},
		{"unclosed code fence", []agent.ChatEvent{{Type: "text", Delta: "```go\npartial"}, {Type: "error", ErrorMessage: "specific failure"}}, true, "```go\npartial"},
		{"partial batch", []agent.ChatEvent{{Type: "text", Delta: "partial answer"}, {Type: "error", ErrorMessage: "specific failure"}}, false, "partial answer"},
		{"partial stream", []agent.ChatEvent{{Type: "text", Delta: "partial answer"}, {Type: "done", Message: &agent.Message{Content: "authoritative partial"}, ErrorMessage: "specific failure"}}, true, "authoritative partial"},
		{"duplicate terminals", []agent.ChatEvent{{Type: "error", ErrorMessage: "specific failure"}, {Type: "done", ErrorMessage: "specific failure"}}, false, ""},
		{"failed silence", []agent.ChatEvent{{Type: "text", Delta: noReplyToken}, {Type: "error", ErrorMessage: "specific failure"}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &streamScript{}
			if tc.streaming {
				script.streamTSs = []string{"stream.1"}
			}
			srv := newStreamServer(t, script)
			bot := newBotWithStream(t, &scriptedMgr{events: tc.events}, srv)
			bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
			body := strings.Join(script.postedMD, "\n") + script.lastUpdateMD
			if tc.partial != "" && strings.Contains(script.lastPostMD, tc.partial) {
				t.Fatalf("diagnostic mixed with partial markdown: %q", script.lastPostMD)
			}
			if strings.Count(body, "specific failure") != 1 || !strings.Contains(body, tc.partial) || strings.Contains(body, noReplyToken) {
				t.Fatalf("unexpected error response: %q", body)
			}
		})
	}
}

type errorStartMgr struct{ scriptedMgr }

func (*errorStartMgr) ChatOneShot(context.Context, string, string, agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	return nil, errors.New("specific startup failure")
}
func TestSendToAgentShowsStartupError(t *testing.T) {
	script := &streamScript{}
	bot := newBotWithStream(t, &errorStartMgr{}, newStreamServer(t, script))
	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if !strings.Contains(script.lastPostMD, "specific startup failure") {
		t.Fatal(script.lastPostMD)
	}
}

func TestSendToAgentErrorSurvivesUpdateFallback(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}, failUpdate: true}
	mgr := &scriptedMgr{events: []agent.ChatEvent{{Type: "text", Delta: "partial result"}, {Type: "error", ErrorMessage: "specific failure"}}}
	bot := newBotWithStream(t, mgr, newStreamServer(t, script))
	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if !strings.Contains(strings.Join(script.postedMD, "\n"), "partial result") || !strings.Contains(script.lastPostMD, "specific failure") {
		t.Fatal(script.lastPostMD)
	}
}

func TestSendToAgentUnexpectedEOF(t *testing.T) {
	for _, partial := range []string{"", "partial answer"} {
		script := &streamScript{}
		events := []agent.ChatEvent{}
		if partial != "" {
			events = append(events, agent.ChatEvent{Type: "text", Delta: partial})
		}
		bot := newBotWithStream(t, &scriptedMgr{events: events}, newStreamServer(t, script))
		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
		if !strings.Contains(script.lastPostMD, "完了通知") {
			t.Fatal(script.lastPostMD)
		}
	}
}
func TestSlackChatErrorStandardCredentials(t *testing.T) {
	for _, secret := range []string{"github_pat_abcdef12345", "ASIA1234567890123456", "GOCSPX-privateSecret", `private_key="privateKeyValue"`, "AKIA1234567890123456", "AIzaabcdef12345", "AWS_SECRET_ACCESS_KEY=privateAwsSecret"} {
		if strings.Contains(slackChatError(secret), "privateKeyValue") || strings.Contains(slackChatError(secret), "privateAwsSecret") || strings.Contains(slackChatError(secret), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if len(slackChatError(strings.Repeat("あ&", 1200))) >= slackMaxMsgLen {
		t.Fatal("diagnostic splits its code fence")
	}
}

func TestSlackChatErrorEscapedDetailBudget(t *testing.T) {
	text := slackChatError(strings.Repeat("あ&", 1200))
	parts := strings.Split(text, "```\n")
	detail := strings.TrimSuffix(parts[1], "\n```")
	if len(detail) > 2000 {
		t.Fatalf("detail has %d bytes", len(detail))
	}
}
