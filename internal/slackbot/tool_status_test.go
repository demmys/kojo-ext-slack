package slackbot

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToolStatusText(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{
			name:      "claude bash prefers description",
			toolName:  "Bash",
			toolInput: `{"command":"git status","description":"Show working tree status"}`,
			want:      "Running command: Show working tree status",
		},
		{
			name:      "claude bash without description shows command",
			toolName:  "Bash",
			toolInput: `{"command":"git status"}`,
			want:      "Running command: git status",
		},
		{
			name:      "claude bash blank description falls back to command",
			toolName:  "Bash",
			toolInput: `{"command":"git status","description":"  \n "}`,
			want:      "Running command: git status",
		},
		{
			name:      "sub-agent shows description, never prompt",
			toolName:  "Agent",
			toolInput: `{"description":"Find callers","prompt":"very long brief"}`,
			want:      "Running sub-agent: Find callers",
		},
		{
			name:      "sub-agent without description",
			toolName:  "Task",
			toolInput: `{"prompt":"very long brief"}`,
			want:      "Running sub-agent…",
		},
		{
			name:      "read ignores description field",
			toolName:  "Read",
			toolInput: `{"file_path":"/foo/bar.go","description":"x"}`,
			want:      "Reading file: /foo/bar.go",
		},
		{
			name:      "codex shell raw command",
			toolName:  "shell",
			toolInput: "ls -la /tmp",
			want:      "Running command: ls -la /tmp",
		},
		{
			name:      "codex shell cat reads file",
			toolName:  "shell",
			toolInput: "cat internal/slackbot/bot.go",
			want:      "Running command: cat internal/slackbot/bot.go",
		},
		{
			name:      "claude read shows path",
			toolName:  "Read",
			toolInput: `{"file_path":"/foo/bar.go"}`,
			want:      "Reading file: /foo/bar.go",
		},
		{
			name:      "claude edit shows path",
			toolName:  "Edit",
			toolInput: `{"file_path":"/foo/bar.go","old_string":"a","new_string":"b"}`,
			want:      "Editing file: /foo/bar.go",
		},
		{
			name:      "claude grep shows pattern",
			toolName:  "Grep",
			toolInput: `{"pattern":"toolStatus","path":"internal"}`,
			want:      "Searching code: toolStatus",
		},
		{
			name:      "websearch shows query",
			toolName:  "WebSearch",
			toolInput: `{"query":"golang slack stream"}`,
			want:      "Searching the web: golang slack stream",
		},
		{
			name:      "no input falls back to label",
			toolName:  "Bash",
			toolInput: "",
			want:      "Running command…",
		},
		{
			name:      "unknown tool keeps raw name in plain text",
			toolName:  "slack/post_message",
			toolInput: `{"text":"hi"}`,
			want:      "Using slack/post_message…",
		},
		{
			name:      "unknown mcp tool with underscores",
			toolName:  "mcp__slack__post_message",
			toolInput: `{"text":"hi"}`,
			want:      "Using mcp__slack__post_message…",
		},
		{
			name:      "empty tool name",
			toolName:  "",
			toolInput: "",
			want:      "Working…",
		},
		{
			name:      "malformed json falls back to label",
			toolName:  "Bash",
			toolInput: `{not json`,
			want:      "Running command…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolStatusText(tt.toolName, tt.toolInput)
			if got != tt.want {
				t.Errorf("toolStatusText(%q, %q) = %q, want %q", tt.toolName, tt.toolInput, got, tt.want)
			}
		})
	}
}

func TestToolStatusIndicator(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{
			name:      "known tool with detail in code span",
			toolName:  "shell",
			toolInput: "git status",
			want:      "\n\n_⏳ Running command:_ `git status`",
		},
		{
			name:      "bash description then command on next line",
			toolName:  "Bash",
			toolInput: `{"command":"git status","description":"巡回スクリプト結果確認"}`,
			want:      "\n\n_⏳ Running command:_ 巡回スクリプト結果確認  \n`git status`",
		},
		{
			name:      "bash description is single-lined and backtick-free",
			toolName:  "Bash",
			toolInput: `{"command":"echo hi","description":"check\n` + "`x`" + ` value"}`,
			want:      "\n\n_⏳ Running command:_ check 'x' value  \n`echo hi`",
		},
		{
			name:      "bash without description keeps command code span",
			toolName:  "Bash",
			toolInput: `{"command":"git status"}`,
			want:      "\n\n_⏳ Running command:_ `git status`",
		},
		{
			name:      "bash description without command",
			toolName:  "Bash",
			toolInput: `{"description":"Show status"}`,
			want:      "\n\n_⏳ Running command:_ Show status",
		},
		{
			name:      "sub-agent description only",
			toolName:  "Task",
			toolInput: `{"description":"Find callers","prompt":"brief"}`,
			want:      "\n\n_⏳ Running sub-agent:_ Find callers",
		},
		{
			name:      "codex shell has no description",
			toolName:  "shell",
			toolInput: `{"description":"not json for codex"}`,
			want:      "\n\n_⏳ Running command:_ `{\"description\":\"not json for codex\"}`",
		},
		{
			name:      "known tool without detail",
			toolName:  "Bash",
			toolInput: "",
			want:      "\n\n_⏳ Running command…_",
		},
		{
			name:      "unknown mcp tool name goes in code span not italics",
			toolName:  "mcp__slack__post_message",
			toolInput: `{"text":"hi"}`,
			want:      "\n\n_⏳ Using_ `mcp__slack__post_message`",
		},
		{
			name:      "empty tool name",
			toolName:  "",
			toolInput: "",
			want:      "\n\n_⏳ Working…_",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolStatusIndicator(tt.toolName, tt.toolInput)
			if got != tt.want {
				t.Errorf("toolStatusIndicator(%q, %q) = %q, want %q", tt.toolName, tt.toolInput, got, tt.want)
			}
		})
	}
}

func TestCleanToolDetail(t *testing.T) {
	t.Run("collapses whitespace and newlines", func(t *testing.T) {
		got := cleanToolDetail("git   commit\n  -m  'x'")
		want := "git commit -m 'x'"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("neutralizes backticks", func(t *testing.T) {
		got := cleanToolDetail("echo `date`")
		if strings.Contains(got, "`") {
			t.Errorf("backtick not neutralized: %q", got)
		}
	})

	t.Run("truncates long detail on rune boundary", func(t *testing.T) {
		long := strings.Repeat("あ", toolDetailMaxLen+50)
		got := cleanToolDetail(long)
		runes := []rune(got)
		// toolDetailMaxLen runes plus the ellipsis.
		if len(runes) != toolDetailMaxLen+1 {
			t.Errorf("expected %d runes, got %d", toolDetailMaxLen+1, len(runes))
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("expected ellipsis suffix, got %q", got)
		}
	})

	t.Run("empty stays empty", func(t *testing.T) {
		if got := cleanToolDetail("   "); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("no invalid utf8 when byte pre-cap cuts mid-rune", func(t *testing.T) {
		// Many spaces then enough multi-byte runes to push past the byte
		// pre-cap (toolDetailMaxLen*4). After whitespace collapsing the
		// result is short, so the final rune-truncation does NOT run —
		// any partial rune left by the byte slice must already be gone.
		input := strings.Repeat(" ", toolDetailMaxLen*4) + strings.Repeat("あ", 50)
		got := cleanToolDetail(input)
		if !utf8.ValidString(got) {
			t.Errorf("result contains invalid UTF-8: %q", got)
		}
		if strings.Contains(got, "�") {
			t.Errorf("result contains replacement char: %q", got)
		}
	})
}
