package slackbot

import (
	"context"
	"encoding/json"
	"time"

	"github.com/slack-go/slack"
)

// forwardedFilesFromPayload extracts files carried inside the attachments of
// a message event (Slack "forward"/"share message" puts the original
// message's files at event.attachments[].files, not event.files).
//
// slack-go's typed events cannot be used here: slack.Attachment has no Files
// field (v0.21.0), so the files are discarded during decoding. The raw
// Socket Mode payload is decoded instead. Any decode failure yields nil — a
// forwarded file is best-effort and must never block the message itself.
func forwardedFilesFromPayload(raw json.RawMessage) []slack.File {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Event struct {
			Attachments []struct {
				Files []slack.File `json:"files"`
			} `json:"attachments"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	var files []slack.File
	for _, a := range payload.Event.Attachments {
		files = append(files, a.Files...)
	}
	return files
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
