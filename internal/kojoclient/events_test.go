package kojoclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// eventConn is one accepted connection of the fake events stream.
type eventConn struct {
	lines chan any
	drop  chan struct{}
	gone  chan struct{}
}

func (c *eventConn) send(v any) { c.lines <- v }

// eventsServer is a fake kojo: it serves ext-chat/events and records the
// background-cancel and handoff-ack requests the client sends back.
type eventsServer struct {
	t *testing.T

	// refuse makes the next n connection attempts fail.
	refuse  atomic.Int32
	dials   atomic.Int32
	conns   chan *eventConn
	cancels chan map[string]any
	acks    chan map[string]any
	// silent suppresses the stream's opening ping.
	silent atomic.Bool
}

func newEventsServer(t *testing.T) *eventsServer {
	return &eventsServer{
		t:       t,
		conns:   make(chan *eventConn, 16),
		cancels: make(chan map[string]any, 16),
		acks:    make(chan map[string]any, 16),
	}
}

func (s *eventsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	switch r.URL.Path {
	case extChat + "/events":
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testToken {
			s.t.Errorf("events request = %s, Authorization %q", r.Method, r.Header.Get("Authorization"))
		}
		s.dials.Add(1)
		if s.refuse.Load() > 0 {
			s.refuse.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, apiErrorBody("unavailable", "starting"))
			return
		}
		conn := &eventConn{lines: make(chan any, 1024), drop: make(chan struct{}), gone: make(chan struct{})}
		defer close(conn.gone)
		startStream(w)
		if !s.silent.Load() {
			writeLine(s.t, w, map[string]any{"type": "ping"})
		}
		s.conns <- conn
		for {
			select {
			case v := <-conn.lines:
				writeLine(s.t, w, v)
			case <-conn.drop:
				return
			case <-r.Context().Done():
				return
			}
		}
	case extChat + "/background-cancel":
		s.cancels <- body
		_, _ = io.WriteString(w, `{"ok":true,"cancelled":true}`)
	case extChat + "/handoff-ack":
		s.acks <- body
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

type backgroundTurn struct {
	agentID string
	key     string
	events  <-chan agent.ChatEvent
	cancel  func()
}

type abandonedNotice struct {
	agentID string
	key     string
	pending int
	reason  string
}

type goalResume struct{ key, userID, command string }

// fakeHandler stands in for the Slack bot.
type fakeHandler struct {
	turns     chan backgroundTurn
	abandoned chan abandonedNotice
	// hold, when set, keeps HandleKeyedBackgroundTurn from returning.
	hold chan struct{}
	// drain makes the handler read its events itself, as the bot does.
	drain bool
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{turns: make(chan backgroundTurn, 16), abandoned: make(chan abandonedNotice, 16)}
}

func (h *fakeHandler) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	h.turns <- backgroundTurn{agentID, sessionKey, events, cancel}
	if h.drain {
		for range events {
		}
	}
	if h.hold != nil {
		<-h.hold
	}
}

func (h *fakeHandler) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	h.abandoned <- abandonedNotice{agentID, sessionKey, pending, reason}
}

// resumingHandler is a handler that can also resume goals, like the bot.
type resumingHandler struct {
	*fakeHandler
	resumes chan goalResume
}

func (h *resumingHandler) ResumeGoal(sessionKey, userID, command string) {
	h.resumes <- goalResume{sessionKey, userID, command}
}

// fakeReservation is an arrival slot without a source barrier.
type fakeReservation struct {
	mu          sync.Mutex
	activations []string
	activateErr error
	released    atomic.Int32
}

func (r *fakeReservation) Activate(_ context.Context, prompt, expectedHolder string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activations = append(r.activations, prompt+"|"+expectedHolder)
	return r.activateErr
}

func (r *fakeReservation) Release() { r.released.Add(1) }

func (r *fakeReservation) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.activations...)
}

// barrierReservation also has the source-completion barrier.
type barrierReservation struct {
	fakeReservation
	complete chan error
}

func (r *barrierReservation) WaitSourceComplete(ctx context.Context) error {
	select {
	case err := <-r.complete:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// connect registers h and returns the first accepted connection.
func connect(t *testing.T, c *Client, srv *eventsServer, h agent.KeyedBackgroundHandler) *eventConn {
	t.Helper()
	stop := c.RegisterKeyedBackgroundHandler(testAgent, h)
	t.Cleanup(stop)
	return waitFor(t, srv.conns, "the events stream to connect")
}

func TestEventsBackgroundTurn(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "background_turn", "turnId": "t1", "sessionKey": testWire})
	conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "text", "delta": "task finished"}})
	conn.send(map[string]any{"type": "ping"})
	conn.send(map[string]any{"type": "background_event", "turnId": "unknown", "event": map[string]any{"type": "text", "delta": "stray"}})
	conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "done", "backgroundTasksPending": 2}})
	conn.send(map[string]any{"type": "background_end", "turnId": "t1"})
	conn.send(map[string]any{"type": "background_end", "turnId": "t1"})

	turn := waitFor(t, h.turns, "the background turn")
	if turn.agentID != testAgent || turn.key != testKey {
		t.Fatalf("turn = %q %q, want %q %q", turn.agentID, turn.key, testAgent, testKey)
	}
	got := collect(t, turn.events)
	if want := []string{"text", "done"}; !reflect.DeepEqual(eventTypes(got), want) {
		t.Fatalf("events = %v, want %v", eventTypes(got), want)
	}
	if got[0].Delta != "task finished" || got[1].BackgroundTasksPending != 2 {
		t.Fatalf("events = %+v", got)
	}
	select {
	case body := <-srv.cancels:
		t.Fatalf("a finished turn was cancelled: %v", body)
	default:
	}
}

func TestEventsBackgroundTurnCancel(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "background_turn", "turnId": "t1", "sessionKey": testWire})
	turn := waitFor(t, h.turns, "the background turn")
	// A `!stop` reaches cancel from the bot's event loop; twice is one stop.
	turn.cancel()
	turn.cancel()
	body := waitFor(t, srv.cancels, "background-cancel")
	if want := map[string]any{"turnId": "t1"}; !reflect.DeepEqual(body, want) {
		t.Fatalf("cancel body = %v, want %v", body, want)
	}
	conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "done", "errorMessage": agent.ErrMsgCancelled}})
	conn.send(map[string]any{"type": "background_end", "turnId": "t1"})
	got := collect(t, turn.events)
	if len(got) != 1 || got[0].ErrorMessage != agent.ErrMsgCancelled {
		t.Fatalf("events = %+v, want the cancelled terminal", got)
	}
	select {
	case body := <-srv.cancels:
		t.Fatalf("second background-cancel: %v", body)
	case <-time.After(50 * time.Millisecond):
	}
}

// The handler must not be left reading a turn whose stream is gone.
func TestEventsDisconnectMidTurnClosesTheTurn(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "background_turn", "turnId": "t1", "sessionKey": testWire})
	conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "text", "delta": "partial"}})
	turn := waitFor(t, h.turns, "the background turn")
	if first := waitFor(t, turn.events, "the first event"); first.Delta != "partial" {
		t.Fatalf("first event = %+v", first)
	}
	close(conn.drop)
	if got := collect(t, turn.events); len(got) != 0 {
		t.Fatalf("events after the disconnect = %+v, want the channel closed with no terminal", got)
	}

	// The stream comes back and a turn ID from the old connection means
	// nothing on the new one.
	next := waitFor(t, srv.conns, "the reconnect")
	next.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "text", "delta": "stale"}})
	next.send(map[string]any{"type": "background_turn", "turnId": "t2", "sessionKey": "C2:5.6"})
	next.send(map[string]any{"type": "background_end", "turnId": "t2"})
	second := waitFor(t, h.turns, "a turn on the new connection")
	if second.key != "ag_1:slack:C2:5.6" {
		t.Fatalf("key = %q", second.key)
	}
	if got := collect(t, second.events); len(got) != 0 {
		t.Fatalf("events = %+v, want none", got)
	}
}

// The handler waits in the thread FIFO before it reads anything; the reader
// must keep serving everything else on the stream meanwhile.
func TestEventsSlowHandlerDoesNotStallTheStream(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := &resumingHandler{fakeHandler: newFakeHandler(), resumes: make(chan goalResume, 4)}
	h.hold = make(chan struct{})
	defer close(h.hold)
	conn := connect(t, c, srv, h)

	const n = 500
	conn.send(map[string]any{"type": "background_turn", "turnId": "t1", "sessionKey": testWire})
	for range n {
		conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "text", "delta": "x"}})
	}
	conn.send(map[string]any{"type": "background_event", "turnId": "t1", "event": map[string]any{"type": "done"}})
	conn.send(map[string]any{"type": "background_end", "turnId": "t1"})
	conn.send(map[string]any{"type": "goal_resume", "sessionKey": "C9:1.1", "userId": "U1", "command": "resume"})

	// Nothing has read the turn yet, and the resume behind it still arrives.
	waitFor(t, h.resumes, "the goal resume queued behind an unread turn")
	turn := waitFor(t, h.turns, "the background turn")
	if got := collect(t, turn.events); len(got) != n+1 || got[n].Type != "done" {
		t.Fatalf("got %d events, want %d ending in done", len(got), n+1)
	}
}

func TestEventsAbandonedNotice(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "background_abandoned", "sessionKey": testWire, "pending": 3, "reason": "process exited"})
	got := waitFor(t, h.abandoned, "the abandoned notice")
	if want := (abandonedNotice{testAgent, testKey, 3, "process exited"}); got != want {
		t.Fatalf("notice = %+v, want %+v", got, want)
	}
}

// kojo used to call the handler synchronously, so the notice that a
// session's tasks were abandoned could not overtake the turn before it.
func TestEventsDeliveriesForOneSessionKeepTheirOrder(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	h.drain = true
	h.hold = make(chan struct{})
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "background_turn", "turnId": "t1", "sessionKey": testWire})
	conn.send(map[string]any{"type": "background_end", "turnId": "t1"})
	conn.send(map[string]any{"type": "background_abandoned", "sessionKey": testWire, "pending": 1, "reason": "exit"})
	conn.send(map[string]any{"type": "background_abandoned", "sessionKey": "C2:5.6", "pending": 2, "reason": "exit"})

	waitFor(t, h.turns, "the background turn")
	// Another session is not held up by this one.
	if got := waitFor(t, h.abandoned, "the other session's notice"); got.key != "ag_1:slack:C2:5.6" {
		t.Fatalf("first notice = %+v, want the other session's", got)
	}
	select {
	case got := <-h.abandoned:
		t.Fatalf("notice %+v overtook the turn still being delivered", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(h.hold)
	if got := waitFor(t, h.abandoned, "the notice after the turn"); got.key != testKey || got.pending != 1 {
		t.Fatalf("notice = %+v", got)
	}
}

func TestEventsGoalResume(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := &resumingHandler{fakeHandler: newFakeHandler(), resumes: make(chan goalResume, 4)}
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "goal_resume", "sessionKey": testWire, "userId": "U123", "command": "resume"})
	got := waitFor(t, h.resumes, "the goal resume")
	if want := (goalResume{testKey, "U123", "resume"}); got != want {
		t.Fatalf("resume = %+v, want %+v", got, want)
	}
}

func TestEventsGoalResumeWithoutResumerIsDropped(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	conn := connect(t, c, srv, h)

	conn.send(map[string]any{"type": "goal_resume", "sessionKey": testWire, "userId": "U123", "command": "resume"})
	conn.send(map[string]any{"type": "some_future_event"})
	conn.send("not an object")
	// The stream is still being served afterwards.
	conn.send(map[string]any{"type": "background_abandoned", "sessionKey": testWire, "pending": 1})
	waitFor(t, h.abandoned, "the event after the dropped ones")
}

func TestEventsHandoffActivate(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())
	res := &fakeReservation{}
	id := c.registerReservation(res)

	conn.send(map[string]any{"type": "handoff_activate", "requestId": "r1", "reservationId": id, "prompt": "continue here", "expectedHolder": "peer-b"})
	ack := waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r1"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
	if got := res.seen(); !reflect.DeepEqual(got, []string{"continue here|peer-b"}) {
		t.Fatalf("activations = %v", got)
	}

	// A refusal travels back as the error kojo's caller sees.
	res.mu.Lock()
	res.activateErr = errors.New("Slack source turn has already ended")
	res.mu.Unlock()
	conn.send(map[string]any{"type": "handoff_activate", "requestId": "r2", "reservationId": id, "prompt": "again"})
	ack = waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r2", "error": "Slack source turn has already ended"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
}

func TestEventsHandoffUnknownReservation(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())

	conn.send(map[string]any{"type": "handoff_activate", "requestId": "r1", "reservationId": "gone", "prompt": "p"})
	ack := waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r1", "error": "Slack arrival reservation is missing"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r2", "reservationId": "gone"})
	ack = waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r2", "error": "Slack arrival reservation is missing"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
}

func TestEventsHandoffWaitSource(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())
	res := &barrierReservation{complete: make(chan error, 1)}
	id := c.registerReservation(res)

	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r1", "reservationId": id})
	select {
	case ack := <-srv.acks:
		t.Fatalf("acknowledged before the source turn completed: %v", ack)
	case <-time.After(100 * time.Millisecond):
	}
	// The wait does not hold up the stream.
	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r0", "reservationId": "gone"})
	if ack := waitFor(t, srv.acks, "handoff-ack"); ack["requestId"] != "r0" {
		t.Fatalf("ack = %v", ack)
	}
	res.complete <- nil
	ack := waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r1"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}

	res.complete <- errors.New("source turn failed to finalize")
	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r2", "reservationId": id})
	ack = waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r2", "error": "source turn failed to finalize"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
}

func TestEventsHandoffWaitSourceWithoutBarrier(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())
	id := c.registerReservation(&fakeReservation{})

	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r1", "reservationId": id})
	ack := waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r1", "error": "origin adapter has no source completion barrier"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
}

// handoff_release must not free the bot's slot: only the bot knows when the
// turn in front of it has finished delivering.
func TestEventsHandoffReleaseLeavesTheSlotToTheBot(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())
	res := &barrierReservation{complete: make(chan error, 1)}
	id := c.registerReservation(res)

	conn.send(map[string]any{"type": "handoff_release", "reservationId": id})
	// The barrier is still answerable after the release.
	res.complete <- nil
	conn.send(map[string]any{"type": "handoff_wait_source", "requestId": "r1", "reservationId": id})
	ack := waitFor(t, srv.acks, "handoff-ack")
	if want := map[string]any{"requestId": "r1"}; !reflect.DeepEqual(ack, want) {
		t.Fatalf("ack = %v, want %v", ack, want)
	}
	if n := res.released.Load(); n != 0 {
		t.Fatalf("Release was called %d times", n)
	}
}

func TestEventsHandoffAckErrorIsBounded(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	conn := connect(t, c, srv, newFakeHandler())
	res := &fakeReservation{activateErr: errors.New(strings.Repeat("é", 2000))}
	id := c.registerReservation(res)

	conn.send(map[string]any{"type": "handoff_activate", "requestId": "r1", "reservationId": id})
	ack := waitFor(t, srv.acks, "handoff-ack")
	text, _ := ack["error"].(string)
	if text == "" || len(text) > ackErrorLimit || strings.Trim(text, "é") != "" {
		t.Fatalf("error is %d bytes, want 1..%d bytes of intact text", len(text), ackErrorLimit)
	}
}

func TestEventsReconnectsUntilStopped(t *testing.T) {
	srv := newEventsServer(t)
	srv.refuse.Store(3)
	c, _ := newTestClient(t, srv)
	h := newFakeHandler()
	stop := c.RegisterKeyedBackgroundHandler(testAgent, h)

	first := waitFor(t, srv.conns, "a connection after the refused attempts")
	if n := srv.dials.Load(); n != 4 {
		t.Fatalf("server saw %d attempts, want 4", n)
	}
	close(first.drop)
	second := waitFor(t, srv.conns, "the reconnect")
	second.send(map[string]any{"type": "background_abandoned", "sessionKey": testWire, "pending": 1})
	waitFor(t, h.abandoned, "an event on the new connection")

	stop()
	waitFor(t, second.gone, "the stream to be closed by stop")
	stop()
	dials := srv.dials.Load()
	time.Sleep(100 * time.Millisecond)
	if n := srv.dials.Load(); n != dials {
		t.Fatalf("the client reconnected after stop (%d -> %d attempts)", dials, n)
	}
}

func TestEventsSilentStreamIsReconnected(t *testing.T) {
	srv := newEventsServer(t)
	srv.silent.Store(true)
	c, _ := newTestClient(t, srv)
	c.idleTimeout = 100 * time.Millisecond
	connect(t, c, srv, newFakeHandler())
	waitFor(t, srv.conns, "a reconnect after the stream went silent")
}

func TestEventsRegistrationForAnotherAgentIsIgnored(t *testing.T) {
	srv := newEventsServer(t)
	c, _ := newTestClient(t, srv)
	stop := c.RegisterKeyedBackgroundHandler("ag_2", newFakeHandler())
	defer stop()
	time.Sleep(100 * time.Millisecond)
	if n := srv.dials.Load(); n != 0 {
		t.Fatalf("the events stream was opened %d times for another agent", n)
	}
}

// A handler stuck behind its thread must not let a turn grow the buffer
// without bound: past the limit the turn is cancelled once, the rest of its
// events are dropped, and the handler sees the stream end without a terminal.
func TestRelayEventsCancelsATurnThatOutrunsItsHandler(t *testing.T) {
	in := make(chan agent.ChatEvent)
	out := make(chan agent.ChatEvent)
	var overflows atomic.Int32
	go relayEvents(in, out, func() { overflows.Add(1) })

	const extra = 10
	for i := 0; i < relayBufferLimit+extra; i++ {
		in <- agent.ChatEvent{Type: "text"}
	}
	in <- agent.ChatEvent{Type: "done"}
	close(in)

	got := 0
	for evt := range out {
		if evt.Type == "done" {
			t.Fatal("the terminal event of an overflowed turn was delivered")
		}
		got++
	}
	if got != relayBufferLimit {
		t.Fatalf("delivered %d events, want the %d that fit", got, relayBufferLimit)
	}
	if n := overflows.Load(); n != 1 {
		t.Fatalf("overflow called %d times, want 1", n)
	}
}
