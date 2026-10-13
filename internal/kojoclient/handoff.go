package kojoclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// handoffReservationTTL is how long an arrival reservation stays addressable
// after its turn's stream ended. kojo asks for the source-completion barrier
// when the handoff settles, which is after the source turn; an hour is the
// lifetime kojo gives its own handoff capability for the turn.
const handoffReservationTTL = time.Hour

// ackErrorLimit is the longest error text kojo accepts in a handoff-ack.
const ackErrorLimit = 1024

// sourceBarrier is the optional reservation capability behind
// handoff_wait_source: block until the source turn has finished delivering.
type sourceBarrier interface {
	WaitSourceComplete(ctx context.Context) error
}

// registerReservation makes r addressable by a fresh ID, which travels to
// kojo as handoffReservationId and comes back on the events stream.
func (c *Client) registerReservation(r agent.HandoffArrivalReservation) string {
	var raw [16]byte
	// crypto/rand.Read never fails on the platforms this runs on.
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	c.mu.Lock()
	c.reservations[id] = r
	c.mu.Unlock()
	return id
}

func (c *Client) dropReservation(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	delete(c.reservations, id)
	c.mu.Unlock()
}

// expireReservation drops the reservation once kojo can no longer ask for it.
func (c *Client) expireReservation(id string) {
	if id == "" {
		return
	}
	time.AfterFunc(c.reservationTTL, func() { c.dropReservation(id) })
}

func (c *Client) reservation(id string) agent.HandoffArrivalReservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reservations[id]
}

// activateReservation runs the arrival turn in the reserved slot.
func (c *Client) activateReservation(ctx context.Context, id, prompt, expectedHolder string) error {
	r := c.reservation(id)
	if r == nil {
		return errors.New("Slack arrival reservation is missing")
	}
	return r.Activate(ctx, prompt, expectedHolder)
}

// waitReservationSource blocks until the reservation's source turn finished
// delivering, and reports whether it finalized successfully.
func (c *Client) waitReservationSource(ctx context.Context, id string) error {
	r := c.reservation(id)
	if r == nil {
		return errors.New("Slack arrival reservation is missing")
	}
	barrier, ok := r.(sourceBarrier)
	if !ok {
		return errors.New("origin adapter has no source completion barrier")
	}
	return barrier.WaitSourceComplete(ctx)
}

// ackHandoff answers a handoff_activate or handoff_wait_source request.
func (c *Client) ackHandoff(ctx context.Context, requestID string, result error) {
	body := struct {
		RequestID string `json:"requestId"`
		Error     string `json:"error,omitempty"`
	}{RequestID: requestID}
	if result != nil {
		// kojo refuses an acknowledgement whose error is longer than this,
		// and a refused refusal would leave the handoff waiting for nothing.
		body.Error = strings.ToValidUTF8(truncate(result.Error(), ackErrorLimit), "")
	}
	ackCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.call(ackCtx, "handoff-ack", body, nil); err != nil {
		c.logger.Warn("handoff acknowledgement was not delivered", "requestId", requestID, "err", err)
	}
}

// truncate cuts s to at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
