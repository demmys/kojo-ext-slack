package slackbot

import (
	"sync"
	"time"
)

// deliveredMessageTTL bounds how long a handled Slack message timestamp is
// remembered. Slack sends the paired message/app_mention events for one post
// within seconds, and Socket Mode retries an unacknowledged envelope within a
// few minutes; anything older is not a duplicate delivery of the same post.
const deliveredMessageTTL = 10 * time.Minute

// deliveredMessages remembers Slack messages (channel + message ts) that one
// event handler has already committed to. A bot mention posted in a thread
// arrives both as a `message` event and as an `app_mention` event. When the
// thread is eligible for auto-replies, both handlers would otherwise accept
// the same post and run it as two separate agent turns. The zero value is
// ready to use.
type deliveredMessages struct {
	mu   sync.Mutex
	seen map[string]time.Time // key: "channel:ts" -> handled at
	now  func() time.Time     // tests override; nil means time.Now
}

func deliveredMessageKey(channel, messageTS string) string {
	return channel + ":" + messageTS
}

func (d *deliveredMessages) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// handled reports whether the message was already committed to by another
// event within the TTL. Messages without a timestamp are never deduplicated.
func (d *deliveredMessages) handled(channel, messageTS string) bool {
	if messageTS == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	at, ok := d.seen[deliveredMessageKey(channel, messageTS)]
	return ok && d.clock().Sub(at) < deliveredMessageTTL
}

// mark records that the message has been committed to (accepted as a turn or
// consumed as a command). Expired entries are pruned on the way so the map
// stays bounded by the traffic of one TTL window.
func (d *deliveredMessages) mark(channel, messageTS string) {
	if messageTS == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	for k, at := range d.seen {
		if now.Sub(at) >= deliveredMessageTTL {
			delete(d.seen, k)
		}
	}
	d.seen[deliveredMessageKey(channel, messageTS)] = now
}
