package slackbot

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

var slackErrorSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[^\s"'<>]+`),
	regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`),
	regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|secret[_-]?access[_-]?key|private[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|authorization)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,"'&<>]+)`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]+|xox[baprs]-[A-Za-z0-9-]+|gh[opusr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|(?:AKIA|ASIA)[A-Z0-9]{16}|GOCSPX-[A-Za-z0-9_-]+|AIza[A-Za-z0-9_-]+)\b`),
}

// Backend diagnostics are untrusted text: bound them, redact common credentials,
// and prevent Slack mentions/markup from being interpreted as error content.
func slackChatError(detail string) string {
	hint := ""
	switch {
	case strings.Contains(detail, "this conversation already has a goal"):
		hint = "\n続きは通常の返信か `!goal resume`。別の目標に置き換える場合は `!goal clear` の後に `!goal <目標>` を送ってください。"
	case strings.Contains(detail, "active goal without a runner"):
		hint = "\n続きは通常の返信か `!goal resume`。停止したままにする場合は `!goal pause` を送ってください。"
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "⚠️ 処理に失敗しました。詳細なエラーは返されませんでした。"
	}
	for _, re := range slackErrorSecrets {
		detail = re.ReplaceAllString(detail, "[REDACTED]")
	}
	detail = strings.ReplaceAll(detail, noReplyToken, "[control token]")
	runes := []rune(detail)
	if len(runes) > 1200 {
		detail = string(runes[:1200]) + "…（省略）"
	}
	detail = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "'").Replace(detail)
	if len(detail) > 2000 {
		cut := 2000 - len("…（省略）")
		for !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "…（省略）"
	}
	return "⚠️ 処理に失敗しました。\n```\n" + detail + "\n```" + hint
}

// A slow stream finalization must not consume the diagnostic delivery budget.
func (b *Bot) postChatError(channel, threadTS, detail string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), chunkPostTimeout(1))
	defer cancel()
	return b.postMessage(ctx, channel, threadTS, slackChatError(detail))
}
