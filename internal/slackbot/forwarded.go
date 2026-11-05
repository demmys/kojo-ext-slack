package slackbot

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo-ext-slack/internal/uploadpath"
)

// forwardedMessage is one forwarded/shared message carried in a message
// event's attachments. Slack "forward"/"share message" puts the original
// message (its text, author, source channel, permalink and files) at
// event.attachments[], not at the top level of the event.
type forwardedMessage struct {
	IsShare     bool         `json:"is_share"`
	IsMsgUnfurl bool         `json:"is_msg_unfurl"`
	AuthorID    string       `json:"author_id"`
	AuthorName  string       `json:"author_name"`
	ChannelID   string       `json:"channel_id"`
	ChannelName string       `json:"channel_name"`
	FromURL     string       `json:"from_url"`
	TS          flexTS       `json:"ts"`
	Text        string       `json:"text"`
	Fallback    string       `json:"fallback"`
	Files       []slack.File `json:"files"`

	// Linked marks a message referenced by a permalink (Slack's own
	// unfurl, or a link in the text resolved by the bot) rather than a
	// forward. FetchError is set when a linked message could not be read.
	Linked     bool   `json:"-"`
	FetchError string `json:"-"`
}

// isForward reports whether the attachment represents a forwarded message
// (as opposed to e.g. an ordinary link unfurl).
func (m forwardedMessage) isForward() bool {
	return m.IsShare || m.IsMsgUnfurl || len(m.Files) > 0
}

// forwardedMessagesFromPayload extracts forwarded messages from the raw
// Socket Mode payload.
//
// slack-go's typed events cannot be used here: slack.Attachment has no Files
// field (v0.21.0), so the files are discarded during decoding. The raw
// payload is decoded instead. Any decode failure yields nil — forwarded
// content is best-effort and must never block the message itself.
func forwardedMessagesFromPayload(raw json.RawMessage) []forwardedMessage {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Event struct {
			Attachments []forwardedMessage `json:"attachments"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	var msgs []forwardedMessage
	for _, a := range payload.Event.Attachments {
		if a.isForward() {
			a.Linked = a.IsMsgUnfurl && !a.IsShare
			msgs = append(msgs, a)
		}
	}
	return msgs
}

// forwardedFilesFromPayload returns only the files of forwarded messages.
func forwardedFilesFromPayload(raw json.RawMessage) []slack.File {
	return forwardedFiles(forwardedMessagesFromPayload(raw))
}

func forwardedFiles(msgs []forwardedMessage) []slack.File {
	var files []slack.File
	for _, m := range msgs {
		files = append(files, m.Files...)
	}
	return files
}

// forwardedChannelLookupTimeout bounds the conversations.info lookup used to
// name the source channel of a forward.
const forwardedChannelLookupTimeout = 5 * time.Second

// resolveForwardedChannelNames fills ChannelName for forwards that carry only
// a channel ID. Failures keep the ID; the agent still sees where it came from.
func (b *Bot) resolveForwardedChannelNames(ctx context.Context, msgs []forwardedMessage) {
	if b.api == nil {
		return
	}
	for i := range msgs {
		m := &msgs[i]
		if m.ChannelName != "" || m.ChannelID == "" {
			continue
		}
		lookupCtx, cancel := context.WithTimeout(ctx, forwardedChannelLookupTimeout)
		info, err := b.api.GetConversationInfoContext(lookupCtx, &slack.GetConversationInfoInput{ChannelID: m.ChannelID})
		cancel()
		if err != nil || info == nil {
			b.logger.Debug("slack conversations.info failed for forward", "channel", m.ChannelID, "err", err)
			continue
		}
		m.ChannelName = info.Name
	}
}

// appendForwardedMessages renders forwarded messages into the user text so
// the agent can tell a forward from a direct post and knows its origin.
// Slack markup (<@U…>, links) is left intact for SlackToPlain to resolve.
func appendForwardedMessages(text string, msgs []forwardedMessage) string {
	for _, m := range msgs {
		var sb strings.Builder
		label := "Forwarded message"
		if m.Linked {
			label = "Linked message"
		}
		sb.WriteString("[" + label + "]\n")
		var from []string
		switch {
		case m.ChannelName != "" && m.ChannelID != "":
			from = append(from, "#"+m.ChannelName+" ("+m.ChannelID+")")
		case m.ChannelName != "":
			from = append(from, "#"+m.ChannelName)
		case m.ChannelID != "":
			from = append(from, m.ChannelID)
		}
		switch {
		case m.AuthorID != "":
			from = append(from, "by <@"+m.AuthorID+">")
		case m.AuthorName != "":
			from = append(from, "by "+m.AuthorName)
		}
		if t := formatSlackTS(string(m.TS)); t != "" {
			from = append(from, "at "+t)
		}
		if len(from) > 0 {
			sb.WriteString("from: " + strings.Join(from, " ") + "\n")
		}
		if m.FromURL != "" {
			sb.WriteString("link: " + m.FromURL + "\n")
		}
		if m.FetchError != "" {
			sb.WriteString("status: " + describeLinkedFetchError(m.FetchError) + "\n")
		}
		body := m.Text
		if strings.TrimSpace(body) == "" && len(m.Files) == 0 {
			body = m.Fallback
		}
		if strings.TrimSpace(body) != "" {
			sb.WriteString("text:\n" + body + "\n")
		}
		if len(m.Files) > 0 {
			names := make([]string, 0, len(m.Files))
			for _, f := range m.Files {
				name := uploadpath.SanitizeName(f.Name)
				if name == "" {
					name = "attachment"
				}
				names = append(names, name)
			}
			sb.WriteString("files: " + strings.Join(names, ", ") + "\n")
		}
		sb.WriteString("[/" + label + "]")
		if strings.TrimSpace(text) != "" {
			text += "\n\n"
		}
		text += sb.String()
	}
	return text
}

func formatSlackTS(ts string) string {
	if ts == "" {
		return ""
	}
	sec, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	return time.Unix(n, 0).Format("2006-01-02 15:04 MST")
}

// mergeForwardedFiles appends forwarded files to the message's own files,
// skipping entries whose ID is already present (the same file may appear both
// directly and inside a share). Files without an ID are kept as-is.
func mergeForwardedFiles(files, forwarded []slack.File) []slack.File {
	if len(forwarded) == 0 {
		return files
	}
	seen := make(map[string]bool, len(files)+len(forwarded))
	for _, f := range files {
		if f.ID != "" {
			seen[f.ID] = true
		}
	}
	for _, f := range forwarded {
		if f.ID != "" {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
		}
		files = append(files, f)
	}
	return files
}

// fileInfoTimeout bounds each files.info lookup. The download path may run
// synchronously on the Socket Mode dispatch goroutine, so a stalled lookup
// must not hold it indefinitely.
const fileInfoTimeout = 15 * time.Second

// resolveFileDownloadURLs fills in url_private_download for files that carry
// only an ID (forwarded attachments may arrive as partial file objects) by
// asking files.info with the bot token. Lookup failures leave the entry
// untouched so downloadSlackFiles reports it as an error to the agent.
func (b *Bot) resolveFileDownloadURLs(ctx context.Context, files []slack.File) []slack.File {
	if b.api == nil {
		return files
	}
	for i := range files {
		f := &files[i]
		if f.URLPrivateDownload != "" || f.ID == "" {
			continue
		}
		lookupCtx, cancel := context.WithTimeout(ctx, fileInfoTimeout)
		info, _, _, err := b.api.GetFileInfoContext(lookupCtx, f.ID, 0, 0)
		cancel()
		if err != nil || info == nil {
			b.logger.Warn("slack files.info failed", "fileID", f.ID, "err", err)
			continue
		}
		if f.Name == "" {
			f.Name = info.Name
		}
		if f.Mimetype == "" {
			f.Mimetype = info.Mimetype
		}
		if f.Size == 0 {
			f.Size = info.Size
		}
		f.URLPrivateDownload = info.URLPrivateDownload
	}
	return files
}

// flexTS accepts an attachment ts given either as a JSON string or a number
// (legacy attachments allow both). Unparseable values decode to empty so one
// odd field never drops the whole forwarded payload.
type flexTS string

func (t *flexTS) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*t = flexTS(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*t = flexTS(n.String())
		return nil
	}
	*t = ""
	return nil
}
