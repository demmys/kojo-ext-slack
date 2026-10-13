// Package kojoclient implements, over kojo's ext-chat HTTP API, the
// interfaces the Slack bot used to call on kojo in-process: ChatOneShot and
// the optional steering, question, goal, background and handoff capabilities.
//
// The bot keeps working with full session keys,
// "<agentID>:slack:<channel>:<threadTS>". On the wire a key is relative to
// the package, "<channel>:<threadTS>"; kojo maps the package ID "slack" back
// onto the same namespace. The client converts in both directions and
// refuses a key it cannot convert rather than sending it.
package kojoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// streamIdleTimeout is how long a stream may stay silent. kojo writes a ping
// every 15s, so a minute of nothing means the connection is dead even if TCP
// has not noticed.
const streamIdleTimeout = 75 * time.Second

// streamLineLimit bounds one NDJSON line. A terminal event carries the whole
// reply and its tool output, which can be large.
const streamLineLimit = 16 << 20

// Client talks to the kojo API with the extension's own token, on behalf of
// one agent.
type Client struct {
	base    string
	token   string
	agentID string
	logger  *slog.Logger
	// stream has no overall timeout: a turn runs as long as the agent needs.
	// Liveness comes from the server's ping lines instead.
	stream *http.Client
	short  *http.Client

	// Timing knobs, fields so tests do not have to wait for the real values.
	idleTimeout    time.Duration
	busyBackoff    func(attempt int) time.Duration
	cancelGrace    time.Duration
	reconnectMin   time.Duration
	reconnectMax   time.Duration
	reservationTTL time.Duration

	mu           sync.Mutex
	reservations map[string]agent.HandoffArrivalReservation
}

// New returns a client for agentID. base is KOJO_API_BASE and token the
// extension token.
func New(base, token, agentID string, logger *slog.Logger) *Client {
	return &Client{
		base:           strings.TrimRight(base, "/"),
		token:          token,
		agentID:        agentID,
		logger:         logger.With("component", "kojoclient", "agent", agentID),
		stream:         &http.Client{},
		short:          &http.Client{Timeout: 30 * time.Second},
		idleTimeout:    streamIdleTimeout,
		busyBackoff:    func(attempt int) time.Duration { return time.Duration(attempt+1) * time.Second },
		cancelGrace:    cancelTerminalGrace,
		reconnectMin:   time.Second,
		reconnectMax:   30 * time.Second,
		reservationTTL: handoffReservationTTL,
		reservations:   make(map[string]agent.HandoffArrivalReservation),
	}
}

// keyPrefix is the namespace kojo gives the package ID "slack". It is the
// same prefix the built-in bot used, which is what keeps existing thread
// sessions resolving.
func (c *Client) keyPrefix() string { return c.agentID + ":slack:" }

// wireKey converts a full session key into the package-relative key the API
// takes. A key outside this agent's Slack namespace is a caller bug, and
// sending it would address some other session, so it is an error.
func (c *Client) wireKey(agentID, sessionKey string) (string, error) {
	if agentID != c.agentID {
		return "", fmt.Errorf("kojo client is bound to agent %q, not %q", c.agentID, agentID)
	}
	suffix, ok := strings.CutPrefix(sessionKey, c.keyPrefix())
	if !ok || suffix == "" {
		return "", fmt.Errorf("session key %q is not in the %q namespace", sessionKey, c.keyPrefix())
	}
	return suffix, nil
}

// fullKey converts a package-relative key received from kojo back into the
// full session key the bot works with.
func (c *Client) fullKey(suffix string) string { return c.keyPrefix() + suffix }

func (c *Client) path(route string) string {
	p := "/api/v1/agents/" + url.PathEscape(c.agentID) + "/ext-chat"
	if route != "" {
		p += "/" + route
	}
	return p
}

// APIError is a non-2xx answer from kojo. Its text is the server's message
// alone: the bot posts it into the Slack thread, exactly as it posted the
// in-process error. errors.Is matches the sentinel the error code stands for.
type APIError struct {
	Status   int
	Code     string
	Message  string
	sentinel error
}

func (e *APIError) Error() string { return e.Message }

func (e *APIError) Unwrap() error { return e.sentinel }

// codeSentinels maps the wire error codes onto the errors the bot tests for.
var codeSentinels = map[string]error{
	"not_busy":             agent.ErrAgentNotBusy,
	"steer_unsupported":    agent.ErrSteerUnsupported,
	"steer_uncertain":      agent.ErrSteerDeliveryUncertain,
	"question_not_found":   agent.ErrQuestionNotFound,
	"invalid_answers":      agent.ErrInvalidQuestionAnswer,
	"goal_owner_forbidden": agent.ErrGoalOwnerForbidden,
	"background_not_found": agent.ErrBackgroundSessionNotFound,
}

func decodeAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || env.Error.Code == "" {
		return &APIError{
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("kojo API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
		}
	}
	out := &APIError{
		Status:   resp.StatusCode,
		Code:     env.Error.Code,
		Message:  env.Error.Message,
		sentinel: codeSentinels[env.Error.Code],
	}
	if out.Message == "" {
		if out.sentinel != nil {
			out.Message = out.sentinel.Error()
		} else {
			out.Message = fmt.Sprintf("kojo API returned HTTP %d %s", resp.StatusCode, env.Error.Code)
		}
	}
	return out
}

func apiErrorCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// call runs one short control request and decodes its JSON answer into out
// (which may be nil).
func (c *Client) call(ctx context.Context, route string, body, out any) error {
	req, err := c.newRequest(ctx, http.MethodPost, c.path(route), body)
	if err != nil {
		return err
	}
	resp, err := c.short.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Secrets fetches the extension's secret settings for this agent. The MCP
// server uses it; the service already has them in KOJO_EXT_SECRETS.
func (c *Client) Secrets(ctx context.Context) (map[string]string, error) {
	u := "/api/v1/extension/secrets"
	if c.agentID != "" {
		u += "?agentId=" + url.QueryEscape(c.agentID)
	}
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.short.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(resp)
	}
	var out struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}
