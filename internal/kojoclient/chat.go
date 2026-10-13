package kojoclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/loppo-llc/kojo-ext-slack/internal/chathistory"
)

// busyAttempts is how many times a turn refused with "busy" is retried.
const busyAttempts = 5

// cancelTerminalGrace is how long a turn stopped through ext-chat/cancel may
// take to deliver its terminal event before the stream is dropped.
const cancelTerminalGrace = 30 * time.Second

// cancelRequestTimeout bounds the ext-chat/cancel request itself.
const cancelRequestTimeout = 10 * time.Second

// chatRequest is the ext-chat body. Everything but sessionKey and message is
// optional and omitted when unset; kojo rejects unknown fields, so a field
// is only ever sent when the bot asked for the behaviour behind it.
type chatRequest struct {
	SessionKey               string                       `json:"sessionKey"`
	Message                  string                       `json:"message"`
	SystemPromptExtra        string                       `json:"systemPromptExtra,omitempty"`
	History                  []chathistory.HistoryMessage `json:"history,omitempty"`
	HistorySelfUserID        string                       `json:"historySelfUserId,omitempty"`
	Attachments              []agent.MessageAttachment    `json:"attachments,omitempty"`
	Goal                     *agent.GoalRequest           `json:"goal,omitempty"`
	GoalUserID               string                       `json:"goalUserId,omitempty"`
	InteractiveQuestions     bool                         `json:"interactiveQuestions,omitempty"`
	PreserveTerminalOnCancel bool                         `json:"preserveTerminalOnCancel,omitempty"`
	LingerBackgroundTasks    bool                         `json:"lingerBackgroundTasks,omitempty"`
	ForceFreshSession        bool                         `json:"forceFreshSession,omitempty"`
	ExpectedHolderPeer       string                       `json:"expectedHolderPeer,omitempty"`
	HandoffReservationID     string                       `json:"handoffReservationId,omitempty"`
}

// ChatOneShot runs one turn on the session opts.SessionKey names. The
// returned channel has the in-process contract: deltas, at most one terminal
// done/error event, then it is closed. A stream that dies first closes the
// channel without a terminal event, which the bot reports as a failed turn.
// Cancelling ctx stops the turn.
func (c *Client) ChatOneShot(ctx context.Context, agentID, message string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	key, err := c.wireKey(agentID, opts.SessionKey)
	if err != nil {
		return nil, err
	}
	body := chatRequest{
		SessionKey:               key,
		Message:                  message,
		SystemPromptExtra:        opts.SystemPromptExtra,
		History:                  opts.History,
		HistorySelfUserID:        opts.HistorySelfUserID,
		Attachments:              opts.Attachments,
		Goal:                     opts.Goal,
		GoalUserID:               opts.GoalUserID,
		InteractiveQuestions:     opts.InteractiveQuestions,
		PreserveTerminalOnCancel: opts.PreserveTerminalOnCancel,
		LingerBackgroundTasks:    opts.LingerBackgroundTasks,
		ForceFreshSession:        opts.ForceFreshSession,
		ExpectedHolderPeer:       opts.ExpectedHolderPeer,
	}
	if opts.HandoffArrivalReservation != nil {
		body.HandoffReservationID = c.registerReservation(opts.HandoffArrivalReservation)
	}

	// The request outlives ctx: a stopped turn still has a terminal event to
	// deliver (the authoritative partial reply and the number of background
	// tasks that survive the stop), and that only arrives on a stream that is
	// still open. Until the turn has started, cancelling ctx simply aborts.
	reqCtx, abort := context.WithCancel(context.WithoutCancel(ctx))
	stopAbort := context.AfterFunc(ctx, abort)
	resp, err := c.openChat(reqCtx, body)
	if err == nil && !stopAbort() {
		_ = resp.Body.Close()
		err = ctx.Err()
	}
	if err != nil {
		stopAbort()
		abort()
		c.dropReservation(body.HandoffReservationID)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	out := make(chan agent.ChatEvent, 64)
	// A goal command that carries no message is sent next to a turn that may
	// be running on the same session. ext-chat/cancel addresses a session,
	// not a request, so using it for such a command could stop that turn.
	cancellable := message != "" || opts.Goal == nil
	go func() {
		defer close(out)
		defer abort()
		// The arrival slot can be asked for after the stream has ended (see
		// handoffReservationTTL), so it is not dropped here.
		defer c.expireReservation(body.HandoffReservationID)
		c.pumpChat(ctx, reqCtx, abort, resp.Body, key, cancellable, out)
	}()
	return out, nil
}

// openChat posts the turn and returns the open event stream. A "busy"
// refusal is retried briefly: kojo holds a session until the previous turn's
// backend has wound down, which is slightly after a stopped turn's stream
// ends and the bot's thread FIFO lets the next message through. The FIFO
// admits one turn per session at a time, so the retry only ever waits for
// this bot's own previous turn and cannot reorder anything.
func (c *Client) openChat(ctx context.Context, body chatRequest) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, http.MethodPost, c.path(""), body)
		if err != nil {
			return nil, err
		}
		resp, err := c.stream.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		err = decodeAPIError(resp)
		_ = resp.Body.Close()
		if apiErrorCode(err) != "busy" || attempt >= busyAttempts-1 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.busyBackoff(attempt)):
		}
	}
}

// pumpChat copies the turn's events from body to out until the stream ends.
// abort cancels reqCtx, which tears the request down.
func (c *Client) pumpChat(ctx, reqCtx context.Context, abort context.CancelFunc, body io.ReadCloser, key string, cancellable bool, out chan<- agent.ChatEvent) {
	defer body.Close()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-ctx.Done():
			c.stopTurn(reqCtx, abort, key, cancellable, ended)
		case <-ended:
		}
	}()

	idle := time.AfterFunc(c.idleTimeout, abort)
	defer idle.Stop()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), streamLineLimit)
	terminal := false
	for sc.Scan() {
		idle.Reset(c.idleTimeout)
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var evt agent.ChatEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			c.logger.Warn("undecodable ext-chat event skipped", "err", err)
			continue
		}
		// Exactly one terminal event ends a turn; anything after it is not
		// part of the contract the bot was written against.
		if evt.Type == "ping" || terminal {
			continue
		}
		terminal = evt.Type == "done" || evt.Type == "error"
		select {
		case out <- evt:
		case <-reqCtx.Done():
			return
		}
	}
	if err := sc.Err(); err != nil && !terminal && ctx.Err() == nil {
		c.logger.Warn("ext-chat stream ended before the turn completed", "sessionKey", key, "err", err)
	}
}

// stopTurn stops the running turn after its context was cancelled. Asking
// kojo to cancel keeps the stream open for the terminal event; if that is
// not possible the request is aborted, which stops the turn as well but
// loses the terminal event.
func (c *Client) stopTurn(reqCtx context.Context, abort context.CancelFunc, key string, cancellable bool, ended <-chan struct{}) {
	if !cancellable {
		abort()
		return
	}
	cancelCtx, done := context.WithTimeout(reqCtx, cancelRequestTimeout)
	err := c.call(cancelCtx, "cancel", struct {
		SessionKey string `json:"sessionKey"`
	}{key}, nil)
	done()
	if err != nil {
		if reqCtx.Err() == nil {
			c.logger.Debug("ext-chat cancel failed; dropping the stream instead", "sessionKey", key, "err", err)
		}
		abort()
		return
	}
	grace := time.NewTimer(c.cancelGrace)
	defer grace.Stop()
	select {
	case <-grace.C:
		c.logger.Warn("cancelled turn did not end in time; dropping the stream", "sessionKey", key)
		abort()
	case <-ended:
	}
}
