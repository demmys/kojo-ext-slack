package slackbot

import (
	"strings"
	"testing"
)

func TestSlackToPlain(t *testing.T) {
	// Mock resolver that maps U12345 → "Alice"
	resolver := func(id string) string {
		if id == "U12345" {
			return "Alice"
		}
		return id
	}

	tests := []struct {
		name    string
		input   string
		want    string
		resolve UserResolver
	}{
		{
			name:  "link with text",
			input: `Check <https://example.com|this link>`,
			want:  `Check this link (https://example.com)`,
		},
		{
			name:  "bare link",
			input: `Visit <https://example.com>`,
			want:  `Visit https://example.com`,
		},
		{
			name:  "channel mention",
			input: `Go to <#C12345|general>`,
			want:  `Go to #general`,
		},
		{
			name:    "user mention without resolver",
			input:   `Hello <@U12345>`,
			want:    `Hello @U12345`,
			resolve: nil,
		},
		{
			name:    "user mention with resolver",
			input:   `Hello <@U12345>`,
			want:    `Hello @Alice`,
			resolve: resolver,
		},
		{
			name:    "mixed formatting with resolver",
			input:   `<@U12345> posted <https://example.com|a link> in <#C12345|random>`,
			want:    `@Alice posted a link (https://example.com) in #random`,
			resolve: resolver,
		},
		{
			name:  "plain text unchanged",
			input: `Hello world`,
			want:  `Hello world`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlackToPlain(tt.input, tt.resolve)
			if got != tt.want {
				t.Errorf("SlackToPlain(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripBotMention(t *testing.T) {
	botID := "U99999"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "mention at start",
			input: "<@U99999> help me",
			want:  "help me",
		},
		{
			name:  "mention in middle",
			input: "hey <@U99999> help me",
			want:  "hey  help me",
		},
		{
			name:  "mention at end",
			input: "help me <@U99999>",
			want:  "help me",
		},
		{
			name:  "multiple mentions",
			input: "<@U99999> do <@U99999> this",
			want:  "do  this",
		},
		{
			name:  "no mention",
			input: "hello there",
			want:  "hello there",
		},
		{
			name:  "only mention",
			input: "<@U99999>",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripBotMention(tt.input, botID)
			if got != tt.want {
				t.Errorf("StripBotMention(%q, %q) = %q, want %q", tt.input, botID, got, tt.want)
			}
		})
	}
}

func TestPlainToSlackPreservesCodeWhileConvertingMarkdown(t *testing.T) {
	in := "## Heading\n**bold** ~~gone~~ [link](https://example.com) `**code**`\n```go\n**block**\n```"
	want := "*Heading*\n*bold* ~gone~ <https://example.com|link> `**code**`\n```go\n**block**\n```"
	if got := PlainToSlack(in); got != want {
		t.Fatalf("PlainToSlack() = %q, want %q", got, want)
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int // expected number of chunks
	}{
		{
			name:   "short message",
			text:   "hello",
			maxLen: 100,
			want:   1,
		},
		{
			name:   "exact length",
			text:   "12345",
			maxLen: 5,
			want:   1,
		},
		{
			name:   "needs split",
			text:   strings.Repeat("a", 100),
			maxLen: 40,
			want:   3,
		},
		{
			name:   "split at paragraph",
			text:   strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 30),
			maxLen: 50,
			want:   2,
		},
		{
			name:   "zero maxLen defaults to 3000",
			text:   "short",
			maxLen: 0,
			want:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := SplitMessage(tt.text, tt.maxLen)
			if len(chunks) != tt.want {
				t.Errorf("SplitMessage() returned %d chunks, want %d", len(chunks), tt.want)
			}
			// Verify all text is preserved
			joined := strings.Join(chunks, "")
			if joined != tt.text {
				t.Errorf("SplitMessage() lost content: joined length %d, original %d", len(joined), len(tt.text))
			}
			// Verify no chunk exceeds maxLen (use default if 0)
			limit := tt.maxLen
			if limit <= 0 {
				limit = 3000
			}
			for i, chunk := range chunks {
				if len(chunk) > limit {
					t.Errorf("chunk %d exceeds maxLen: %d > %d", i, len(chunk), limit)
				}
			}
		})
	}
}
