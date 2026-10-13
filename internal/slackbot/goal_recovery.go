package slackbot

import (
	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"strings"
)

// ResumeGoal re-enters the normal Slack admission and streaming path so
// recovery replies go back to the original channel/thread, not main WebUI.
func (b *Bot) ResumeGoal(sessionKey, userID, command string) {
	go b.resumeGoal(sessionKey + "\n" + userID + "\n" + command)
}

func (b *Bot) resumeGoal(key string) {
	// The suffix after newline is an internally supplied guarded resume
	// command. The stable session key itself can never contain a newline.
	sessionKey, rest, ok := strings.Cut(key, "\n")
	if !ok {
		return
	}
	userID, command, ok := strings.Cut(rest, "\n")
	if !ok || len(userID) > 64 || strings.Trim(userID, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return
	}
	q, err := agent.ParseGoalCommand(command)
	if err != nil || q == nil || q.ExpectedGeneration == nil {
		return
	}
	suffix, ok := strings.CutPrefix(sessionKey, b.agentID+":slack:")
	if !ok {
		return
	}
	channel, thread, ok := strings.Cut(suffix, ":")
	if !ok || channel == "" {
		return
	}
	b.processIncomingWithAttachmentsMode(b.ctx, channel, thread, "", command, userID, nil, false)
}
