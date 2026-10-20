package slackbot

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// reSlackMessageLink matches Slack message permalinks in raw message text:
// https://<ws>.slack.com/archives/<channel>/p<sec><usec>[?thread_ts=…]
var reSlackMessageLink = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.slack\.com/archives/([CDG][A-Z0-9]+)/p(\d{10})(\d{6})(\?[^\s|>]*)?`)

// maxLinkedMessages caps how many message links in one post are resolved, so a
// pasted list of permalinks cannot fan out into many API calls and downloads.
const maxLinkedMessages = 3

// linkedMessageLookupTimeout bounds each conversations.history/replies call.
const linkedMessageLookupTimeout = 10 * time.Second

type slackMessageRef struct {
	URL       string
	ChannelID string
	TS        string
	ThreadTS  string
}

// slackMessageRefs extracts unique message permalinks from raw Slack text.
func slackMessageRefs(text string) []slackMessageRef {
	var refs []slackMessageRef
	seen := map[string]bool{}
	for _, m := range reSlackMessageLink.FindAllStringSubmatch(text, -1) {
		ref := slackMessageRef{URL: m[0], ChannelID: m[1], TS: m[2] + "." + m[3]}
		if m[4] != "" {
			if q, err := url.ParseQuery(strings.TrimPrefix(strings.ReplaceAll(m[4], "&amp;", "&"), "?")); err == nil {
				ref.ThreadTS = q.Get("thread_ts")
			}
		}
		key := ref.ChannelID + "/" + ref.TS
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	return refs
}

func hasSlackMessageLinks(text string) bool {
	return reSlackMessageLink.MatchString(text)
}

// resolveLinkedMessages fetches messages referenced by permalinks in text so
// the agent receives their content and files. Links already represented by an
// attachment (forward or Slack's own unfurl) are skipped. Fetch failures —
// typically not_in_channel when the bot is not a member of the source
// channel — are returned as entries carrying FetchError so the agent is told
// the content could not be retrieved instead of silently missing it.
func (b *Bot) resolveLinkedMessages(ctx context.Context, text string, existing []forwardedMessage) []forwardedMessage {
	refs := slackMessageRefs(text)
	if len(refs) == 0 {
		return nil
	}
	covered := map[string]bool{}
	for _, m := range existing {
		if m.ChannelID != "" && m.TS != "" {
			covered[m.ChannelID+"/"+string(m.TS)] = true
		}
	}
	var out []forwardedMessage
	for _, ref := range refs {
		if covered[ref.ChannelID+"/"+ref.TS] {
			continue
		}
		if len(out) >= maxLinkedMessages {
			break
		}
		out = append(out, b.fetchLinkedMessage(ctx, ref))
	}
	return out
}

func (b *Bot) fetchLinkedMessage(ctx context.Context, ref slackMessageRef) forwardedMessage {
	fm := forwardedMessage{Linked: true, ChannelID: ref.ChannelID, TS: flexTS(ref.TS), FromURL: ref.URL}
	if b.api == nil {
		fm.FetchError = "slack client unavailable"
		return fm
	}
	lookupCtx, cancel := context.WithTimeout(ctx, linkedMessageLookupTimeout)
	defer cancel()
	var msgs []slack.Message
	var err error
	if ref.ThreadTS != "" && ref.ThreadTS != ref.TS {
		msgs, _, _, err = b.api.GetConversationRepliesContext(lookupCtx, &slack.GetConversationRepliesParameters{
			ChannelID: ref.ChannelID, Timestamp: ref.ThreadTS,
			Oldest: ref.TS, Latest: ref.TS, Inclusive: true, Limit: 2,
		})
	} else {
		var resp *slack.GetConversationHistoryResponse
		resp, err = b.api.GetConversationHistoryContext(lookupCtx, &slack.GetConversationHistoryParameters{
			ChannelID: ref.ChannelID, Oldest: ref.TS, Latest: ref.TS, Inclusive: true, Limit: 1,
		})
		if resp != nil {
			msgs = resp.Messages
		}
	}
	if err != nil {
		b.logger.Info("slack linked message fetch failed", "channel", ref.ChannelID, "ts", ref.TS, "err", err)
		fm.FetchError = err.Error()
		return fm
	}
	for _, m := range msgs {
		if m.Timestamp != ref.TS {
			continue
		}
		fm.AuthorID = m.User
		if fm.AuthorID == "" {
			fm.AuthorName = m.Username
		}
		fm.Text = m.Text
		fm.Files = append([]slack.File(nil), m.Files...)
		return fm
	}
	fm.FetchError = "message not found"
	return fm
}

func describeLinkedFetchError(err string) string {
	switch err {
	case "not_in_channel", "channel_not_found":
		return fmt.Sprintf("could not fetch (%s): the bot is probably not a member of this channel; ask the user to invite it or paste the content", err)
	case "message not found":
		return "could not fetch: message not found (deleted, or not visible to the bot)"
	default:
		return fmt.Sprintf("could not fetch (%s)", err)
	}
}
