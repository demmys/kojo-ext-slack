package slackbot

import (
	"context"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/loppo-llc/kojo-ext-slack/internal/chathistory"
	"github.com/slack-go/slack/slackevents"
)

func TestDeliveredMessagesTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	d := deliveredMessages{now: func() time.Time { return now }}

	if d.handled("C1", "1.1") {
		t.Fatal("unseen message reported as handled")
	}
	d.mark("C1", "1.1")
	if !d.handled("C1", "1.1") {
		t.Fatal("marked message not reported as handled")
	}
	if d.handled("C2", "1.1") || d.handled("C1", "1.2") {
		t.Fatal("dedup key must include both channel and ts")
	}

	// Messages without a timestamp are never deduplicated.
	d.mark("C1", "")
	if d.handled("C1", "") {
		t.Fatal("empty ts must not be deduplicated")
	}

	now = now.Add(deliveredMessageTTL)
	if d.handled("C1", "1.1") {
		t.Fatal("entry must expire after the TTL")
	}
	d.mark("C1", "2.1")
	if _, ok := d.seen[deliveredMessageKey("C1", "1.1")]; ok {
		t.Fatal("expired entry was not pruned")
	}
}

// newDedupTestBot returns a bot in a channel thread whose last history entry
// is the bot's own reply, so a plain thread post is auto-reply eligible and a
// bot mention arrives as both a message event and an app_mention event.
func newDedupTestBot(t *testing.T, withHistory bool) (*Bot, *countingMgr) {
	t.Helper()
	bot := newTestBot(t, agent.SlackBotConfig{Enabled: true, ThreadReplies: true})
	t.Cleanup(bot.cancel)
	mgr := &countingMgr{}
	bot.mgr = mgr
	if withHistory {
		path := chathistory.HistoryFilePath(bot.agentDataDir, platformSlack, "C1", "thread.1")
		if err := chathistory.AppendMessages(path, []chathistory.HistoryMessage{
			{Platform: platformSlack, ChannelID: "C1", ThreadID: "thread.1", MessageID: "thread.1", UserID: "U123", Text: "<@UBOTTEST> hi"},
			{Platform: platformSlack, ChannelID: "C1", ThreadID: "thread.1", MessageID: "bot.1", UserID: "UBOTTEST", Text: "hello", IsBot: true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return bot, mgr
}

func mentionPairEvents() (*slackevents.MessageEvent, *slackevents.AppMentionEvent) {
	const text = "<@UBOTTEST> is the PRO sheet used too?"
	return &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.1", TimeStamp: "msg.2", Text: text,
	}, &slackevents.AppMentionEvent{
		User: "U123", Channel: "C1",
		ThreadTimeStamp: "thread.1", TimeStamp: "msg.2", Text: text,
	}
}

// waitOneShots waits for at least want turns, then gives a duplicate turn a
// short window to show up before returning the final count.
func waitOneShots(t *testing.T, mgr *countingMgr, want int32) int32 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for mgr.oneShots.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("started %d turns, want at least %d", mgr.oneShots.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return mgr.oneShots.Load()
}

func TestBotMentionInThreadRunsOneTurn(t *testing.T) {
	for _, order := range []string{"app_mention first", "message first"} {
		t.Run(order, func(t *testing.T) {
			bot, mgr := newDedupTestBot(t, true)
			msg, mention := mentionPairEvents()
			if order == "app_mention first" {
				bot.handleAppMentionEvent(context.Background(), mention)
				bot.handleMessageEvent(context.Background(), msg)
			} else {
				bot.handleMessageEvent(context.Background(), msg)
				bot.handleAppMentionEvent(context.Background(), mention)
			}
			if got := waitOneShots(t, mgr, 1); got != 1 {
				t.Fatalf("one Slack post started %d agent turns, want 1", got)
			}
		})
	}
}

// A message event the bot does not accept (no auto-reply history) must not
// claim the post, or the paired app_mention would be silently dropped.
func TestRejectedMessageEventDoesNotSuppressAppMention(t *testing.T) {
	bot, mgr := newDedupTestBot(t, false)
	msg, mention := mentionPairEvents()
	bot.handleMessageEvent(context.Background(), msg)
	bot.handleAppMentionEvent(context.Background(), mention)
	if got := waitOneShots(t, mgr, 1); got != 1 {
		t.Fatalf("started %d agent turns, want 1", got)
	}
}

// `@bot !stop` arrives as both events. Whichever comes first claims the post,
// so the message path must also recognize the command once the bot mention is
// stripped; otherwise it would be steered/queued as chat text and the paired
// app_mention stop would be deduplicated away.
func TestMentionedStopCommandViaMessageEventFirst(t *testing.T) {
	bot, mgr := newDedupTestBot(t, true)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	turn := bot.registerActiveTurnForUser("C1", "thread.1", "U123", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	const text = "<@UBOTTEST> !stop"
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.1", TimeStamp: "msg.3", Text: text,
	})
	bot.handleAppMentionEvent(context.Background(), &slackevents.AppMentionEvent{
		User: "U123", Channel: "C1", ThreadTimeStamp: "thread.1", TimeStamp: "msg.3", Text: text,
	})
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("mentioned !stop did not cancel the active turn")
	}
	if got := mgr.oneShots.Load(); got != 0 {
		t.Fatalf("stop command started %d agent turns, want 0", got)
	}
}
