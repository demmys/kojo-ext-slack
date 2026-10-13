package kojoclient

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// SteerOneShot injects content into the turn running for sessionKey.
func (c *Client) SteerOneShot(ctx context.Context, agentID, sessionKey, content string) error {
	return c.SteerOneShotAsUser(ctx, agentID, sessionKey, content, "")
}

// SteerOneShotAsUser is SteerOneShot with the Slack user who wrote content,
// which kojo needs to enforce that only a goal's owner steers it.
func (c *Client) SteerOneShotAsUser(ctx context.Context, agentID, sessionKey, content, userID string) error {
	key, err := c.wireKey(agentID, sessionKey)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body := struct {
		SessionKey string `json:"sessionKey"`
		Content    string `json:"content"`
		UserID     string `json:"userId,omitempty"`
	}{key, content, userID}
	err = c.call(ctx, "steer", body, nil)
	var apiErr *APIError
	if err == nil || errors.As(err, &apiErr) {
		return err
	}
	return steerTransportError(err)
}

// steerTransportError classifies a steer that got no answer from kojo. The
// bot replays a failed steer as an ordinary follow-up turn, which is only
// safe if kojo never saw it. A failure to connect is that case; once the
// request may have been written, the model may have observed the text, and
// replaying it would deliver the same message twice.
func steerTransportError(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return err
	}
	return fmt.Errorf("%w: %v", agent.ErrSteerDeliveryUncertain, err)
}

// AnswerOneShotQuestion answers a question the agent asked in the turn
// running for sessionKey.
func (c *Client) AnswerOneShotQuestion(ctx context.Context, agentID, sessionKey string, answer agent.QuestionAnswer) error {
	key, err := c.wireKey(agentID, sessionKey)
	if err != nil {
		return err
	}
	body := struct {
		SessionKey string               `json:"sessionKey"`
		Answer     agent.QuestionAnswer `json:"answer"`
	}{key, answer}
	return c.call(ctx, "question", body, nil)
}

// StopIdleGoal fences a goal whose device handoff is still pending for an
// idle session. handled reports that there was such a goal and the stop was
// this user's to make; it can come with an error when the stop is recorded
// but not yet confirmed by the holder.
func (c *Client) StopIdleGoal(ctx context.Context, agentID, sessionKey, userID string) (bool, error) {
	key, err := c.wireKey(agentID, sessionKey)
	if err != nil {
		return false, err
	}
	body := struct {
		SessionKey string `json:"sessionKey"`
		UserID     string `json:"userId"`
	}{key, userID}
	var out struct {
		Handled bool   `json:"handled"`
		Error   string `json:"error"`
	}
	if err := c.call(ctx, "goal-stop", body, &out); err != nil {
		return false, err
	}
	if out.Error != "" {
		return out.Handled, errors.New(out.Error)
	}
	return out.Handled, nil
}

// StopThreadBackgroundTasks stops every background task of the session's
// lingering process (`!stop all`).
func (c *Client) StopThreadBackgroundTasks(ctx context.Context, agentID, sessionKey string) error {
	key, err := c.wireKey(agentID, sessionKey)
	if err != nil {
		return err
	}
	body := struct {
		SessionKey string `json:"sessionKey"`
	}{key}
	return c.call(ctx, "background-stop", body, nil)
}
