package agent

import "testing"

func TestIsNoReplyOnly(t *testing.T) {
	tok := SlackNoReplyToken
	for text, want := range map[string]bool{
		tok: true, tok + tok + tok: true, " " + tok + "\n\n" + tok + " ": true,
		"": false, tok + " ok": false, "`" + tok + "`": false,
	} {
		if IsNoReplyOnly(text) != want {
			t.Errorf("IsNoReplyOnly(%q) != %v", text, want)
		}
	}
	for text, want := range map[string]bool{
		"": true, "[[NO": true, tok + "\n[[NO_RE": true, tok + "x": false, "hi": false,
	} {
		if CouldBeNoReplyOnly(text) != want {
			t.Errorf("CouldBeNoReplyOnly(%q) != %v", text, want)
		}
	}
}
