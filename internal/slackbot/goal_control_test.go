package slackbot

import (
	"context"
	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"strings"
	"testing"
	"time"
)

type goalControlCall struct {
	message string
	opts    agent.OneShotOpts
}
type goalControlMgr struct {
	mockMgr
	calls chan goalControlCall
}

func (m *goalControlMgr) ChatOneShot(_ context.Context, _, message string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	m.calls <- goalControlCall{message, opts}
	ch := make(chan agent.ChatEvent)
	close(ch)
	return ch, nil
}
func TestSlackGoalControlsPreserveUserAndOperation(t *testing.T) {
	for _, command := range []string{"pause", "status", "clear", "budget 100"} {
		t.Run(command, func(t *testing.T) {
			b := newTestBot(t, agent.SlackBotConfig{ThreadReplies: true})
			defer b.cancel()
			m := &goalControlMgr{calls: make(chan goalControlCall, 1)}
			b.mgr = m
			if !b.handleSlackCommand(context.Background(), "C", "T", "M", "UOWNER", "!goal "+command) {
				t.Fatal("not handled")
			}
			select {
			case call := <-m.calls:
				if call.message != "" || call.opts.GoalUserID != "UOWNER" || call.opts.SessionKey != slackSessionKey(b.agentID, "C", "T") || call.opts.Goal == nil || call.opts.Goal.OperationID != "slack:C:M" {
					t.Fatalf("lost control intent: %+v", call)
				}
			case <-time.After(time.Second):
				t.Fatal("no command")
			}
		})
	}
}

type userSteerMgr struct {
	steerTestMgr
	users chan string
}

func (m *userSteerMgr) SteerOneShotAsUser(ctx context.Context, id, key, text, user string) error {
	m.users <- user
	return m.steerTestMgr.SteerOneShot(ctx, id, key, text)
}
func TestSlackLiveSteerPreservesAuthenticatedUser(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	m := &userSteerMgr{steerTestMgr: steerTestMgr{calls: make(chan steerCall, 1)}, users: make(chan string, 1)}
	b.mgr = m
	b.userCache["U123"] = "Alice"
	turn := b.registerActiveTurnForUser("C", "T", "U123", func() {})
	defer b.unregisterActiveTurn("C", "T", turn)
	b.processIncoming(context.Background(), "C", "T", "M", "reply", "U123")
	select {
	case user := <-m.users:
		if user != "U123" {
			t.Fatal(user)
		}
	case <-time.After(time.Second):
		t.Fatal("missing sender")
	}
	waitSteerQueue(t, turn)
}

type ownerSteerMgr struct{ steerTestMgr }

func (m *ownerSteerMgr) SteerOneShotAsUser(ctx context.Context, id, key, text, user string) error {
	if user != "U123" {
		return agent.ErrGoalOwnerForbidden
	}
	return m.steerTestMgr.SteerOneShot(ctx, id, key, text)
}
func TestRejectedGoalReplyDoesNotCloseOwnerSteering(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	m := &ownerSteerMgr{steerTestMgr: steerTestMgr{calls: make(chan steerCall, 1)}}
	b.mgr = m
	b.userCache["U123"] = "Alice"
	b.userCache["U456"] = "Other"
	turn := b.registerActiveTurnForUser("C", "T", "U123", func() {})
	defer b.unregisterActiveTurn("C", "T", turn)
	b.processIncoming(context.Background(), "C", "T", "M1", "unauthorized", "U456")
	waitSteerQueue(t, turn)
	if turn.steerClosed || m.oneShots.Load() != 0 {
		t.Fatal("rejection closed steering or queued a turn")
	}
	b.processIncoming(context.Background(), "C", "T", "M2", "owner correction", "U123")
	select {
	case call := <-m.calls:
		if !strings.Contains(call.content, "owner correction") {
			t.Fatal(call)
		}
	case <-time.After(time.Second):
		t.Fatal("owner blocked by prior rejected input")
	}
	waitSteerQueue(t, turn)
}
