package kojoclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/loppo-llc/kojo-ext-slack/internal/chathistory"
)

const (
	testAgent = "ag_1"
	testKey   = "ag_1:slack:C1:100.200"
	testWire  = "C1:100.200"
	extChat   = "/api/v1/agents/ag_1/ext-chat"
	testToken = "ext-token"
	waitLimit = 5 * time.Second
)

// newTestClient returns a client for srv whose waits are short enough for
// tests.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", testToken, testAgent, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.busyBackoff = func(int) time.Duration { return time.Millisecond }
	c.cancelGrace = 2 * time.Second
	c.reconnectMin = 5 * time.Millisecond
	c.reconnectMax = 20 * time.Millisecond
	return c, srv
}

// recorded is one request a test server saw.
type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
}

// recorder answers every request with a fixed reply and keeps what it saw.
type recorder struct {
	mu     sync.Mutex
	seen   []recorded
	status int
	reply  string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rec := recorded{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Auth: req.Header.Get("Authorization")}
	raw, _ := io.ReadAll(req.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &rec.Body)
	}
	r.mu.Lock()
	r.seen = append(r.seen, rec)
	status, reply := r.status, r.reply
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func (r *recorder) requests() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.seen...)
}

func (r *recorder) only(t *testing.T) recorded {
	t.Helper()
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("server saw %d requests, want 1: %+v", len(seen), seen)
	}
	return seen[0]
}

func apiErrorBody(code, message string) string {
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return string(data)
}

func writeLine(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshal: %v", err)
		return
	}
	_, _ = w.Write(append(data, '\n'))
	w.(http.Flusher).Flush()
}

// collect reads ch until it is closed.
func collect(t *testing.T, ch <-chan agent.ChatEvent) []agent.ChatEvent {
	t.Helper()
	var got []agent.ChatEvent
	deadline := time.After(waitLimit)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, evt)
		case <-deadline:
			t.Fatalf("event channel was not closed; got %+v", got)
		}
	}
}

func eventTypes(events []agent.ChatEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestWireKey(t *testing.T) {
	c := New("http://kojo.invalid", testToken, testAgent, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := c.wireKey(testAgent, testKey)
	if err != nil || got != testWire {
		t.Fatalf("wireKey = %q, %v; want %q", got, err, testWire)
	}
	if full := c.fullKey(got); full != testKey {
		t.Fatalf("fullKey = %q, want %q", full, testKey)
	}
	for name, tc := range map[string]struct{ agentID, key string }{
		"already relative": {testAgent, testWire},
		"other agent key":  {testAgent, "ag_2:slack:C1:100.200"},
		"other namespace":  {testAgent, "ag_1:discord:C1:100.200"},
		"empty suffix":     {testAgent, "ag_1:slack:"},
		"empty":            {testAgent, ""},
		"other agent":      {"ag_2", "ag_2:slack:C1:100.200"},
	} {
		if got, err := c.wireKey(tc.agentID, tc.key); err == nil {
			t.Errorf("%s: wireKey(%q, %q) = %q, want an error", name, tc.agentID, tc.key, got)
		}
	}
}

// A key outside the namespace must never reach kojo, whatever the route.
func TestBadSessionKeyIsNotSent(t *testing.T) {
	rec := &recorder{}
	c, _ := newTestClient(t, rec)
	ctx := context.Background()
	const bad = "C1:100.200"
	calls := map[string]func() error{
		"chat": func() error {
			_, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: bad})
			return err
		},
		"steer":         func() error { return c.SteerOneShot(ctx, testAgent, bad, "x") },
		"steer as user": func() error { return c.SteerOneShotAsUser(ctx, testAgent, bad, "x", "U1") },
		"question": func() error {
			return c.AnswerOneShotQuestion(ctx, testAgent, bad, agent.QuestionAnswer{RequestID: "q"})
		},
		"goal stop": func() error {
			_, err := c.StopIdleGoal(ctx, testAgent, bad, "U1")
			return err
		},
		"background stop": func() error { return c.StopThreadBackgroundTasks(ctx, testAgent, bad) },
		"wrong agent":     func() error { return c.SteerOneShot(ctx, "ag_2", "ag_2:slack:C1:1.2", "x") },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s: want an error for a key outside the namespace", name)
		}
	}
	if seen := rec.requests(); len(seen) != 0 {
		t.Fatalf("server saw %d requests, want none: %+v", len(seen), seen)
	}
}

func TestSteer(t *testing.T) {
	rec := &recorder{reply: `{"ok":true}`}
	c, _ := newTestClient(t, rec)
	if err := c.SteerOneShotAsUser(context.Background(), testAgent, testKey, "more", "U123"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	got := rec.only(t)
	if got.Method != http.MethodPost || got.Path != extChat+"/steer" || got.Auth != "Bearer "+testToken {
		t.Fatalf("request = %+v", got)
	}
	want := map[string]any{"sessionKey": testWire, "content": "more", "userId": "U123"}
	if !reflect.DeepEqual(got.Body, want) {
		t.Fatalf("body = %v, want %v", got.Body, want)
	}
}

func TestSteerWithoutUserOmitsUserID(t *testing.T) {
	rec := &recorder{reply: `{"ok":true}`}
	c, _ := newTestClient(t, rec)
	if err := c.SteerOneShot(context.Background(), testAgent, testKey, "more"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	want := map[string]any{"sessionKey": testWire, "content": "more"}
	if got := rec.only(t).Body; !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v, want %v", got, want)
	}
}

func TestQuestion(t *testing.T) {
	rec := &recorder{reply: `{"ok":true}`}
	c, _ := newTestClient(t, rec)
	answer := agent.QuestionAnswer{RequestID: "req1", Answers: map[string]any{"q1": "yes"}}
	if err := c.AnswerOneShotQuestion(context.Background(), testAgent, testKey, answer); err != nil {
		t.Fatalf("question: %v", err)
	}
	got := rec.only(t)
	if got.Path != extChat+"/question" {
		t.Fatalf("path = %q", got.Path)
	}
	want := map[string]any{"sessionKey": testWire, "answer": map[string]any{"requestId": "req1", "answers": map[string]any{"q1": "yes"}}}
	if !reflect.DeepEqual(got.Body, want) {
		t.Fatalf("body = %v, want %v", got.Body, want)
	}
}

func TestStopIdleGoal(t *testing.T) {
	for name, tc := range map[string]struct {
		reply       string
		wantHandled bool
		wantErr     string
	}{
		"handled":            {`{"handled":true}`, true, ""},
		"nothing to stop":    {`{"handled":false}`, false, ""},
		"handled with error": {`{"handled":true,"error":"stop recorded; holder unreachable"}`, true, "stop recorded; holder unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{reply: tc.reply}
			c, _ := newTestClient(t, rec)
			handled, err := c.StopIdleGoal(context.Background(), testAgent, testKey, "U123")
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if (err == nil) != (tc.wantErr == "") || (err != nil && err.Error() != tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			got := rec.only(t)
			want := map[string]any{"sessionKey": testWire, "userId": "U123"}
			if got.Path != extChat+"/goal-stop" || !reflect.DeepEqual(got.Body, want) {
				t.Fatalf("request = %+v", got)
			}
		})
	}
}

func TestStopThreadBackgroundTasks(t *testing.T) {
	rec := &recorder{reply: `{"ok":true}`}
	c, _ := newTestClient(t, rec)
	if err := c.StopThreadBackgroundTasks(context.Background(), testAgent, testKey); err != nil {
		t.Fatalf("background stop: %v", err)
	}
	got := rec.only(t)
	want := map[string]any{"sessionKey": testWire}
	if got.Path != extChat+"/background-stop" || !reflect.DeepEqual(got.Body, want) {
		t.Fatalf("request = %+v", got)
	}
}

func TestSecrets(t *testing.T) {
	rec := &recorder{reply: `{"secrets":{"botToken":"xoxb-1","appToken":"xapp-1"}}`}
	c, _ := newTestClient(t, rec)
	got, err := c.Secrets(context.Background())
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	if got["botToken"] != "xoxb-1" || got["appToken"] != "xapp-1" {
		t.Fatalf("secrets = %v", got)
	}
	req := rec.only(t)
	if req.Method != http.MethodGet || req.Path != "/api/v1/extension/secrets" || req.Query != "agentId=ag_1" || req.Auth != "Bearer "+testToken {
		t.Fatalf("request = %+v", req)
	}

	rec.mu.Lock()
	rec.status, rec.reply = http.StatusForbidden, apiErrorBody("forbidden", "secrets are not readable")
	rec.mu.Unlock()
	if _, err := c.Secrets(context.Background()); err == nil || err.Error() != "secrets are not readable" {
		t.Fatalf("err = %v, want the server's message", err)
	}
}

// Every error code kojo answers with, on every route that can produce it.
// The bot decides what to do by errors.Is and posts err.Error() into the
// thread, so both the sentinel and the server's text have to survive.
func TestErrorCodeMapping(t *testing.T) {
	ctx := context.Background()
	routes := map[string]func(c *Client) error{
		"chat": func(c *Client) error {
			_, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
			return err
		},
		"steer": func(c *Client) error { return c.SteerOneShotAsUser(ctx, testAgent, testKey, "x", "U1") },
		"question": func(c *Client) error {
			return c.AnswerOneShotQuestion(ctx, testAgent, testKey, agent.QuestionAnswer{RequestID: "q"})
		},
		"goal-stop": func(c *Client) error {
			_, err := c.StopIdleGoal(ctx, testAgent, testKey, "U1")
			return err
		},
		"background-stop": func(c *Client) error { return c.StopThreadBackgroundTasks(ctx, testAgent, testKey) },
	}
	sentinels := []error{
		agent.ErrAgentNotBusy, agent.ErrSteerUnsupported, agent.ErrSteerDeliveryUncertain,
		agent.ErrQuestionNotFound, agent.ErrInvalidQuestionAnswer, agent.ErrGoalOwnerForbidden,
		agent.ErrBackgroundSessionNotFound,
	}
	cases := []struct {
		route    string
		status   int
		code     string
		sentinel error
	}{
		{"chat", http.StatusNotFound, "not_found", nil},
		{"chat", http.StatusForbidden, "goal_owner_forbidden", agent.ErrGoalOwnerForbidden},
		{"chat", http.StatusBadRequest, "chat_rejected", nil},
		{"chat", http.StatusBadRequest, "bad_request", nil},
		{"chat", http.StatusForbidden, "forbidden", nil},
		{"chat", http.StatusRequestEntityTooLarge, "too_large", nil},
		{"steer", http.StatusConflict, "not_busy", agent.ErrAgentNotBusy},
		{"steer", http.StatusConflict, "steer_unsupported", agent.ErrSteerUnsupported},
		{"steer", http.StatusBadGateway, "steer_uncertain", agent.ErrSteerDeliveryUncertain},
		{"steer", http.StatusForbidden, "goal_owner_forbidden", agent.ErrGoalOwnerForbidden},
		{"steer", http.StatusInternalServerError, "steer_failed", nil},
		{"question", http.StatusNotFound, "question_not_found", agent.ErrQuestionNotFound},
		{"question", http.StatusBadRequest, "invalid_answers", agent.ErrInvalidQuestionAnswer},
		{"question", http.StatusConflict, "not_busy", agent.ErrAgentNotBusy},
		{"question", http.StatusInternalServerError, "question_failed", nil},
		{"goal-stop", http.StatusForbidden, "goal_owner_forbidden", agent.ErrGoalOwnerForbidden},
		{"goal-stop", http.StatusInternalServerError, "goal_stop_failed", nil},
		{"background-stop", http.StatusNotFound, "background_not_found", agent.ErrBackgroundSessionNotFound},
		{"background-stop", http.StatusInternalServerError, "background_stop_failed", nil},
	}
	for _, tc := range cases {
		t.Run(tc.route+"/"+tc.code, func(t *testing.T) {
			const message = "server said so: detail 42"
			rec := &recorder{status: tc.status, reply: apiErrorBody(tc.code, message)}
			c, _ := newTestClient(t, rec)
			err := routes[tc.route](c)
			if err == nil {
				t.Fatal("want an error")
			}
			if err.Error() != message {
				t.Fatalf("error text = %q, want the server's message %q", err.Error(), message)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code || apiErr.Status != tc.status {
				t.Fatalf("APIError = %+v", apiErr)
			}
			for _, s := range sentinels {
				if got, want := errors.Is(err, s), s == tc.sentinel; got != want {
					t.Errorf("errors.Is(err, %q) = %v, want %v", s, got, want)
				}
			}
		})
	}
}

func TestErrorWithoutMessageFallsBackToSentinelText(t *testing.T) {
	rec := &recorder{status: http.StatusConflict, reply: `{"error":{"code":"not_busy"}}`}
	c, _ := newTestClient(t, rec)
	err := c.SteerOneShot(context.Background(), testAgent, testKey, "x")
	if !errors.Is(err, agent.ErrAgentNotBusy) || err.Error() != agent.ErrAgentNotBusy.Error() {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorThatIsNotAnAPIEnvelope(t *testing.T) {
	rec := &recorder{status: http.StatusBadGateway, reply: "upstream unavailable\n"}
	c, _ := newTestClient(t, rec)
	err := c.StopThreadBackgroundTasks(context.Background(), testAgent, testKey)
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func TestSteerRefusedConnectionIsNotUncertain(t *testing.T) {
	c, srv := newTestClient(t, &recorder{})
	srv.Close()
	err := c.SteerOneShot(context.Background(), testAgent, testKey, "x")
	if err == nil {
		t.Fatal("want an error")
	}
	// kojo never saw the text, so the bot may replay it as a new turn.
	if errors.Is(err, agent.ErrSteerDeliveryUncertain) {
		t.Fatalf("a refused connection was reported as uncertain: %v", err)
	}
}

func TestSteerLostAnswerIsUncertain(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	err := c.SteerOneShot(context.Background(), testAgent, testKey, "x")
	// kojo read the request; the model may have seen the text.
	if !errors.Is(err, agent.ErrSteerDeliveryUncertain) {
		t.Fatalf("err = %v, want ErrSteerDeliveryUncertain", err)
	}
	if hits.Load() == 0 {
		t.Fatal("request never reached the server")
	}
}

func TestSteerWithCancelledContextIsNotSent(t *testing.T) {
	rec := &recorder{}
	c, _ := newTestClient(t, rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.SteerOneShot(ctx, testAgent, testKey, "x")
	if !errors.Is(err, context.Canceled) || errors.Is(err, agent.ErrSteerDeliveryUncertain) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(rec.requests()) != 0 {
		t.Fatal("a steer with a cancelled context was sent")
	}
}

// chatServer serves ext-chat with handler and records ext-chat/cancel calls.
type chatServer struct {
	t    *testing.T
	chat func(w http.ResponseWriter, r *http.Request, body map[string]any)

	cancelStatus int
	cancelled    chan map[string]any
	chats        atomic.Int32
}

func newChatServer(t *testing.T, chat func(w http.ResponseWriter, r *http.Request, body map[string]any)) *chatServer {
	return &chatServer{t: t, chat: chat, cancelled: make(chan map[string]any, 8)}
}

func (s *chatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	switch r.URL.Path {
	case extChat:
		s.chats.Add(1)
		s.chat(w, r, body)
	case extChat + "/cancel":
		s.cancelled <- body
		if s.cancelStatus != 0 {
			w.WriteHeader(s.cancelStatus)
			_, _ = io.WriteString(w, apiErrorBody("not_found", "no such route"))
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func TestChatOneShotSendsEverythingAndStreamsEvents(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	auth := make(chan string, 1)
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		bodies <- body
		auth <- r.Header.Get("Authorization")
		startStream(w)
		writeLine(t, w, map[string]any{"type": "ping"})
		writeLine(t, w, map[string]any{"type": "status", "status": "thinking"})
		writeLine(t, w, map[string]any{"type": "text", "delta": "he"})
		_, _ = io.WriteString(w, "\n")
		writeLine(t, w, map[string]any{"type": "ping"})
		writeLine(t, w, map[string]any{"type": "text", "delta": "llo"})
		writeLine(t, w, map[string]any{"type": "done", "message": map[string]any{"content": "hello"}})
		// Nothing may follow the terminal event.
		writeLine(t, w, map[string]any{"type": "text", "delta": "late"})
		writeLine(t, w, map[string]any{"type": "error", "errorMessage": "late"})
	})
	c, _ := newTestClient(t, srv)
	gen := int64(3)
	opts := agent.OneShotOpts{
		SessionKey:               testKey,
		SystemPromptExtra:        "from slack",
		History:                  []chathistory.HistoryMessage{{Platform: "slack", ChannelID: "C1", MessageID: "1.0", UserID: "U1", Text: "earlier"}},
		HistorySelfUserID:        "UBOT",
		Attachments:              []agent.MessageAttachment{{Path: "/up/a.txt", Name: "a.txt", Size: 3, Mime: "text/plain"}},
		Goal:                     &agent.GoalRequest{Action: "set", Objective: "ship", ExpectedGeneration: &gen},
		GoalUserID:               "U123",
		InteractiveQuestions:     true,
		PreserveTerminalOnCancel: true,
		LingerBackgroundTasks:    true,
		ForceFreshSession:        true,
		ExpectedHolderPeer:       "peer-b",
	}
	events, err := c.ChatOneShot(context.Background(), testAgent, "hello?", opts)
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	got := collect(t, events)
	if want := []string{"status", "text", "text", "done"}; !reflect.DeepEqual(eventTypes(got), want) {
		t.Fatalf("events = %v, want %v", eventTypes(got), want)
	}
	if got[1].Delta != "he" || got[2].Delta != "llo" || got[3].Message == nil || got[3].Message.Content != "hello" {
		t.Fatalf("events = %+v", got)
	}
	if a := <-auth; a != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", a)
	}
	want := map[string]any{
		"sessionKey":        testWire,
		"message":           "hello?",
		"systemPromptExtra": "from slack",
		"history": []any{map[string]any{
			"platform": "slack", "channelId": "C1", "threadId": "", "messageId": "1.0",
			"userId": "U1", "userName": "", "text": "earlier", "timestamp": "", "isBot": false,
		}},
		"historySelfUserId":        "UBOT",
		"attachments":              []any{map[string]any{"path": "/up/a.txt", "name": "a.txt", "size": float64(3), "mime": "text/plain"}},
		"goal":                     map[string]any{"action": "set", "objective": "ship", "expectedGeneration": float64(3)},
		"goalUserId":               "U123",
		"interactiveQuestions":     true,
		"preserveTerminalOnCancel": true,
		"lingerBackgroundTasks":    true,
		"forceFreshSession":        true,
		"expectedHolderPeer":       "peer-b",
	}
	if body := <-bodies; !reflect.DeepEqual(body, want) {
		t.Fatalf("body =\n%v\nwant\n%v", body, want)
	}
}

// kojo rejects unknown fields and reserves the optional ones, so a plain
// turn must carry nothing but its key and message.
func TestChatOneShotOmitsUnsetFields(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		bodies <- body
		startStream(w)
		writeLine(t, w, map[string]any{"type": "done"})
	})
	c, _ := newTestClient(t, srv)
	events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	collect(t, events)
	want := map[string]any{"sessionKey": testWire, "message": "hi"}
	if body := <-bodies; !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
}

func TestChatOneShotRetriesBusy(t *testing.T) {
	srv := newChatServer(t, nil)
	srv.chat = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if srv.chats.Load() < 3 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, apiErrorBody("busy", "a turn is already running"))
			return
		}
		startStream(w)
		writeLine(t, w, map[string]any{"type": "done"})
	}
	c, _ := newTestClient(t, srv)
	events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	if got := eventTypes(collect(t, events)); !reflect.DeepEqual(got, []string{"done"}) {
		t.Fatalf("events = %v", got)
	}
	if n := srv.chats.Load(); n != 3 {
		t.Fatalf("server saw %d attempts, want 3", n)
	}
}

func TestChatOneShotGivesUpOnBusy(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, apiErrorBody("busy", "a turn is already running"))
	})
	c, _ := newTestClient(t, srv)
	_, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if apiErrorCode(err) != "busy" || err.Error() != "a turn is already running" {
		t.Fatalf("err = %v", err)
	}
	if n := srv.chats.Load(); n != busyAttempts {
		t.Fatalf("server saw %d attempts, want %d", n, busyAttempts)
	}
}

func TestChatOneShotCancelledWhileWaitingOnBusy(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, apiErrorBody("busy", "a turn is already running"))
	})
	c, _ := newTestClient(t, srv)
	c.busyBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A stream that ends without done/error closes the channel and invents
// nothing: the bot turns exactly that into its "failed turn" message.
func TestChatOneShotStreamEndsWithoutTerminal(t *testing.T) {
	for name, end := range map[string]func(w http.ResponseWriter){
		"clean end": func(http.ResponseWriter) {},
		"connection drop": func(w http.ResponseWriter) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				startStream(w)
				writeLine(t, w, map[string]any{"type": "text", "delta": "partial"})
				end(w)
			})
			c, _ := newTestClient(t, srv)
			events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
			if err != nil {
				t.Fatalf("ChatOneShot: %v", err)
			}
			if got := eventTypes(collect(t, events)); !reflect.DeepEqual(got, []string{"text"}) {
				t.Fatalf("events = %v, want only the partial text", got)
			}
		})
	}
}

func TestChatOneShotSilentStreamIsDropped(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		writeLine(t, w, map[string]any{"type": "text", "delta": "partial"})
		<-r.Context().Done()
	})
	c, _ := newTestClient(t, srv)
	c.idleTimeout = 100 * time.Millisecond
	events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	if got := eventTypes(collect(t, events)); !reflect.DeepEqual(got, []string{"text"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestChatOneShotPingsKeepTheStreamAlive(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		for range 8 {
			time.Sleep(50 * time.Millisecond)
			writeLine(t, w, map[string]any{"type": "ping"})
		}
		writeLine(t, w, map[string]any{"type": "done"})
	})
	c, _ := newTestClient(t, srv)
	c.idleTimeout = 200 * time.Millisecond
	events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	if got := eventTypes(collect(t, events)); !reflect.DeepEqual(got, []string{"done"}) {
		t.Fatalf("events = %v, want done", got)
	}
}

// Cancelling the context stops the turn through ext-chat/cancel and keeps
// reading, so the terminal event of the stopped turn still arrives.
func TestChatOneShotCancelDeliversTerminal(t *testing.T) {
	stop := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		writeLine(t, w, map[string]any{"type": "text", "delta": "partial"})
		select {
		case <-stop:
			writeLine(t, w, map[string]any{"type": "done", "errorMessage": agent.ErrMsgCancelled, "message": map[string]any{"content": "partial"}})
		case <-r.Context().Done():
		}
	})
	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey, PreserveTerminalOnCancel: true})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	if first := waitFor(t, events, "first event"); first.Type != "text" {
		t.Fatalf("first event = %+v", first)
	}
	cancel()
	body := waitFor(t, srv.cancelled, "ext-chat/cancel")
	if want := map[string]any{"sessionKey": testWire}; !reflect.DeepEqual(body, want) {
		t.Fatalf("cancel body = %v, want %v", body, want)
	}
	close(stop)
	got := collect(t, events)
	if len(got) != 1 || got[0].Type != "done" || got[0].ErrorMessage != agent.ErrMsgCancelled {
		t.Fatalf("events after cancel = %+v, want the cancelled terminal", got)
	}
}

func TestChatOneShotCancelFallsBackToDroppingTheStream(t *testing.T) {
	gone := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		writeLine(t, w, map[string]any{"type": "text", "delta": "partial"})
		<-r.Context().Done()
		close(gone)
	})
	srv.cancelStatus = http.StatusNotFound
	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	waitFor(t, events, "first event")
	cancel()
	waitFor(t, srv.cancelled, "ext-chat/cancel")
	if got := collect(t, events); len(got) != 0 {
		t.Fatalf("events after cancel = %+v, want none", got)
	}
	waitFor(t, gone, "the request to be aborted")
}

func TestChatOneShotCancelGraceExpires(t *testing.T) {
	gone := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		writeLine(t, w, map[string]any{"type": "text", "delta": "partial"})
		<-r.Context().Done()
		close(gone)
	})
	c, _ := newTestClient(t, srv)
	c.cancelGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	waitFor(t, events, "first event")
	cancel()
	waitFor(t, srv.cancelled, "ext-chat/cancel")
	if got := collect(t, events); len(got) != 0 {
		t.Fatalf("events after cancel = %+v, want none", got)
	}
	waitFor(t, gone, "the request to be aborted")
}

// A goal command without a message runs next to the session's turn.
// ext-chat/cancel is addressed by session, so it must not be used for it.
func TestChatOneShotGoalCommandIsNeverCancelledBySession(t *testing.T) {
	gone := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		writeLine(t, w, map[string]any{"type": "status", "status": "thinking"})
		<-r.Context().Done()
		close(gone)
	})
	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.ChatOneShot(ctx, testAgent, "", agent.OneShotOpts{SessionKey: testKey, Goal: &agent.GoalRequest{Action: "pause"}, GoalUserID: "U1"})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	waitFor(t, events, "first event")
	cancel()
	collect(t, events)
	waitFor(t, gone, "the request to be aborted")
	select {
	case body := <-srv.cancelled:
		t.Fatalf("ext-chat/cancel was called for a goal command: %v", body)
	default:
	}
}

func TestChatOneShotWithCancelledContext(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		<-r.Context().Done()
	})
	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := &fakeReservation{}
	_, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey, HandoffArrivalReservation: res})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := reservationCount(c); n != 0 {
		t.Fatalf("%d reservations left after a turn that never started", n)
	}
}

// A consumer that stops reading must not pin the pump goroutine forever.
func TestChatOneShotAbandonedConsumerDoesNotLeak(t *testing.T) {
	gone := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		startStream(w)
		for {
			select {
			case <-r.Context().Done():
				close(gone)
				return
			default:
			}
			writeLine(t, w, map[string]any{"type": "text", "delta": "x"})
		}
	})
	c, _ := newTestClient(t, srv)
	c.cancelGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.ChatOneShot(ctx, testAgent, "hi", agent.OneShotOpts{SessionKey: testKey}); err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	cancel()
	waitFor(t, gone, "the request to be aborted")
}

func reservationCount(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reservations)
}

func TestChatOneShotRegistersHandoffReservation(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	finish := make(chan struct{})
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		bodies <- body
		startStream(w)
		<-finish
		writeLine(t, w, map[string]any{"type": "done"})
	})
	c, _ := newTestClient(t, srv)
	c.reservationTTL = 100 * time.Millisecond
	res := &fakeReservation{}
	events, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey, HandoffArrivalReservation: res})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	id, _ := waitFor(t, bodies, "request body")["handoffReservationId"].(string)
	if len(id) != 32 {
		t.Fatalf("handoffReservationId = %q, want 32 hex characters", id)
	}
	if c.reservation(id) != agent.HandoffArrivalReservation(res) {
		t.Fatal("the reservation is not addressable by the ID sent to kojo")
	}
	close(finish)
	collect(t, events)
	// kojo asks for the source barrier after the turn's stream has ended.
	if c.reservation(id) == nil {
		t.Fatal("the reservation was dropped as soon as the stream ended")
	}
	deadline := time.Now().Add(waitLimit)
	for c.reservation(id) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the reservation never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res.released.Load() != 0 {
		t.Fatal("expiry released the bot's slot; only the bot's turn may do that")
	}
}

func TestChatOneShotDropsReservationWhenRefused(t *testing.T) {
	srv := newChatServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, apiErrorBody("chat_rejected", "no"))
	})
	c, _ := newTestClient(t, srv)
	_, err := c.ChatOneShot(context.Background(), testAgent, "hi", agent.OneShotOpts{SessionKey: testKey, HandoffArrivalReservation: &fakeReservation{}})
	if err == nil {
		t.Fatal("want an error")
	}
	if n := reservationCount(c); n != 0 {
		t.Fatalf("%d reservations left after a refused turn", n)
	}
}
