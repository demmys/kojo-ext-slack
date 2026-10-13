package kojoclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// backgroundCancelTimeout bounds an ext-chat/background-cancel request.
const backgroundCancelTimeout = 15 * time.Second

// relayBufferLimit is how many events of one background turn may wait for
// the handler; see relayEvents.
const relayBufferLimit = 16384

// goalResumer is the optional handler capability behind goal_resume. The
// Slack bot implements it; kojo used to reach it through the bot hub.
type goalResumer interface {
	ResumeGoal(sessionKey, userID, command string)
}

// pushEvent is one line of the ext-chat/events stream. Which fields are set
// depends on Type.
type pushEvent struct {
	Type string `json:"type"`

	// background_turn, background_event, background_end
	TurnID string           `json:"turnId"`
	Event  *agent.ChatEvent `json:"event"`

	// Package-relative, like every session key on the wire.
	SessionKey string `json:"sessionKey"`

	// background_abandoned
	Pending int    `json:"pending"`
	Reason  string `json:"reason"`

	// goal_resume
	UserID  string `json:"userId"`
	Command string `json:"command"`

	// handoff_activate, handoff_wait_source, handoff_release
	RequestID      string `json:"requestId"`
	ReservationID  string `json:"reservationId"`
	Prompt         string `json:"prompt"`
	ExpectedHolder string `json:"expectedHolder"`
}

// eventSession is one registration of a keyed background handler: the
// events stream is kept connected for as long as it lasts.
type eventSession struct {
	c *Client
	h agent.KeyedBackgroundHandler
	// ctx ends with the registration.
	ctx context.Context

	mu sync.Mutex
	// tails is the end of each session's delivery queue; see deliver.
	tails map[string]chan struct{}
}

// RegisterKeyedBackgroundHandler makes h the surface for everything kojo
// pushes for this agent's Slack sessions: background turns, abandoned-task
// notices, goal resumes (if h can resume goals) and handoff requests. kojo
// treats the agent as having such a surface exactly while the events stream
// is connected, so the stream is opened here and reconnected until the
// returned func is called.
func (c *Client) RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func() {
	if agentID != c.agentID {
		c.logger.Error("keyed background handler registered for another agent; ignored", "requested", agentID)
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &eventSession{c: c, h: h, ctx: ctx, tails: make(map[string]chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run()
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// run keeps the events stream connected, backing off between attempts. It
// never gives up: without the stream the agent's background results, goal
// resumes and handoffs have nowhere to go.
func (s *eventSession) run() {
	delay := s.c.reconnectMin
	for s.ctx.Err() == nil {
		connected, err := s.read()
		if s.ctx.Err() != nil {
			return
		}
		if connected {
			delay = s.c.reconnectMin
		}
		s.c.logger.Warn("ext-chat events stream is down; reconnecting", "in", delay, "err", err)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(delay):
		}
		if !connected {
			delay = min(delay*2, s.c.reconnectMax)
		}
	}
}

// read serves one connection of the events stream until it ends. connected
// reports whether kojo accepted the stream.
func (s *eventSession) read() (connected bool, err error) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	req, err := s.c.newRequest(ctx, http.MethodGet, s.c.path("events"), nil)
	if err != nil {
		return false, err
	}
	resp, err := s.c.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, decodeAPIError(resp)
	}

	// A background turn cannot survive its connection: kojo matches events
	// to turns per stream. Closing the turn's channel without a terminal
	// event makes the handler report the turn as failed instead of waiting
	// for events that will never come.
	turns := make(map[string]chan<- agent.ChatEvent)
	defer func() {
		for _, in := range turns {
			close(in)
		}
	}()

	idle := time.AfterFunc(s.c.idleTimeout, cancel)
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), streamLineLimit)
	for sc.Scan() {
		idle.Reset(s.c.idleTimeout)
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var evt pushEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			s.c.logger.Warn("undecodable ext-chat push event skipped", "err", err)
			continue
		}
		s.dispatch(evt, turns)
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, errors.New("stream closed by kojo")
}

// dispatch handles one push event. It runs on the stream's reader and must
// not block: everything that can wait (the thread FIFO, a Slack call, a
// request back to kojo) happens on another goroutine.
func (s *eventSession) dispatch(evt pushEvent, turns map[string]chan<- agent.ChatEvent) {
	c := s.c
	switch evt.Type {
	case "background_turn":
		if evt.TurnID == "" || evt.SessionKey == "" {
			return
		}
		if _, dup := turns[evt.TurnID]; dup {
			return
		}
		in := make(chan agent.ChatEvent)
		out := make(chan agent.ChatEvent, 64)
		turns[evt.TurnID] = in
		var once sync.Once
		turnID, key := evt.TurnID, c.fullKey(evt.SessionKey)
		// The bot calls cancel from its Socket Mode loop (a `!stop`), where
		// the in-process cancel was a context cancellation; it must return
		// at once here too.
		cancel := func() { once.Do(func() { go c.cancelBackgroundTurn(turnID) }) }
		go relayEvents(in, out, func() {
			c.logger.Warn("background turn cancelled: its events outran the thread", "turnId", turnID, "sessionKey", key)
			cancel()
		})
		s.deliver(key, func() { s.h.HandleKeyedBackgroundTurn(c.agentID, key, out, cancel) })
	case "background_event":
		if in, ok := turns[evt.TurnID]; ok && evt.Event != nil {
			in <- *evt.Event
		}
	case "background_end":
		if in, ok := turns[evt.TurnID]; ok {
			close(in)
			delete(turns, evt.TurnID)
		}
	case "background_abandoned":
		if evt.SessionKey == "" {
			return
		}
		key, pending, reason := c.fullKey(evt.SessionKey), evt.Pending, evt.Reason
		s.deliver(key, func() { s.h.KeyedBackgroundTasksAbandoned(c.agentID, key, pending, reason) })
	case "goal_resume":
		if evt.SessionKey == "" {
			return
		}
		resumer, ok := s.h.(goalResumer)
		if !ok {
			c.logger.Warn("goal resume dropped: the registered handler cannot resume goals")
			return
		}
		resumer.ResumeGoal(c.fullKey(evt.SessionKey), evt.UserID, evt.Command)
	case "handoff_activate":
		go func() {
			c.ackHandoff(s.ctx, evt.RequestID, c.activateReservation(s.ctx, evt.ReservationID, evt.Prompt, evt.ExpectedHolder))
		}()
	case "handoff_wait_source":
		go func() {
			c.ackHandoff(s.ctx, evt.RequestID, c.waitReservationSource(s.ctx, evt.ReservationID))
		}()
	case "handoff_release":
		// kojo will not activate this slot. There is nothing to do with it
		// here: the bot releases the slot itself when its turn has finished
		// delivering, and releasing it on kojo's word, before that, would let
		// the thread's next turn start while this one is still posting. The
		// registration is kept as well, because the source-completion barrier
		// of the same reservation can still be asked for afterwards.
	}
}

// deliver runs fn after every earlier delivery for the same session has
// returned. In-process, kojo called the handler synchronously, one call per
// session at a time, so an abandoned-tasks notice could not reach the thread
// before the background turn that preceded it; this keeps that order.
func (s *eventSession) deliver(key string, fn func()) {
	s.mu.Lock()
	prev := s.tails[key]
	done := make(chan struct{})
	s.tails[key] = done
	s.mu.Unlock()
	go func() {
		if prev != nil {
			<-prev
		}
		fn()
		s.mu.Lock()
		if s.tails[key] == done {
			delete(s.tails, key)
		}
		s.mu.Unlock()
		close(done)
	}()
}

// relayEvents copies in to out through a buffer and closes out once in is
// closed and drained. The handler may spend a long time in the thread FIFO
// before it reads its first event; the stream's reader must be able to keep
// going meanwhile, or pings, other turns and handoff requests would stall
// behind it.
//
// In-process, a handler that was not reading held the turn back instead. That
// backpressure cannot cross the stream, so the buffer is capped: a turn that
// outruns it is cancelled through overflow and loses the rest of its events,
// which ends out without a terminal event and makes the handler report the
// turn as failed.
func relayEvents(in <-chan agent.ChatEvent, out chan<- agent.ChatEvent, overflow func()) {
	defer close(out)
	var pending []agent.ChatEvent
	dropping := false
	for in != nil || len(pending) > 0 {
		var send chan<- agent.ChatEvent
		var head agent.ChatEvent
		if len(pending) > 0 {
			send, head = out, pending[0]
		}
		select {
		case evt, ok := <-in:
			switch {
			case !ok:
				in = nil
			case dropping:
			case len(pending) >= relayBufferLimit:
				// The select above may have picked this event although the
				// handler was ready to take one: only a handler that is not
				// reading counts as outrun.
				select {
				case out <- pending[0]:
					pending = append(pending[1:], evt)
				default:
					dropping = true
					overflow()
				}
			default:
				pending = append(pending, evt)
			}
		case send <- head:
			pending = pending[1:]
		}
	}
}

// cancelBackgroundTurn asks kojo to abort a background turn.
func (c *Client) cancelBackgroundTurn(turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundCancelTimeout)
	defer cancel()
	err := c.call(ctx, "background-cancel", struct {
		TurnID string `json:"turnId"`
	}{turnID}, nil)
	if err != nil {
		c.logger.Warn("background turn cancel was not delivered", "turnId", turnID, "err", err)
	}
}
