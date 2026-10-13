package agent

import "strings"

// SlackNoReplyToken is a complete assistant response requesting no Slack post.
// A mention inside ordinary prose is not a control response.
const SlackNoReplyToken = "[[NO_REPLY]]"

// IsNoReplyOnly reports whether text consists solely of one or more
// SlackNoReplyToken occurrences and whitespace. It is the transport-side
// equivalent of "every segment was a control token" for streams that lost
// segment boundaries (for example an older holder).
func IsNoReplyOnly(text string) bool {
	rest := strings.TrimSpace(text)
	if rest == "" {
		return false
	}
	for rest != "" {
		if !strings.HasPrefix(rest, SlackNoReplyToken) {
			return false
		}
		rest = strings.TrimSpace(rest[len(SlackNoReplyToken):])
	}
	return true
}

// CouldBeNoReplyOnly reports whether streaming text may still become an
// IsNoReplyOnly response (including empty/whitespace text).
func CouldBeNoReplyOnly(text string) bool { return couldBeNoReplyOnly(text) }

func couldBeNoReplyOnly(text string) bool {
	rest := strings.TrimSpace(text)
	for strings.HasPrefix(rest, SlackNoReplyToken) {
		rest = strings.TrimSpace(rest[len(SlackNoReplyToken):])
	}
	return strings.HasPrefix(SlackNoReplyToken, rest)
}
