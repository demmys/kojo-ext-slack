package slackbot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

// A rejected continuation must fall back without dropping later chunks.
// If legacy delivery also fails, retain the stream and report the failure.
func TestSendToAgentInvalidBlocksFallback(t *testing.T) {
	for _, rejectUpdate := range []bool{false, true} {
		for _, rejectLegacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("updateRejected=%t/legacyRejected=%t", rejectUpdate, rejectLegacy), func(t *testing.T) {
				body := strings.Repeat("商品を検証しました。\n\n", 350) + "**最後の検証結果**"
				chunks := SplitMessage(body, slackMaxMsgLen)
				if len(chunks) < 3 {
					t.Fatal("fixture must span at least three chunks")
				}
				var delivered, legacyAttempts []string
				var notice, deleted bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = r.ParseForm()
					md, text := r.FormValue("markdown_text"), r.FormValue("text")
					switch r.URL.Path {
					case "/chat.update":
						if rejectUpdate {
							fmt.Fprint(w, `{"ok":false,"error":"invalid_blocks"}`)
							return
						}
						delivered = append(delivered, md)
					case "/chat.postMessage":
						if r.FormValue("channel") != "C1" || r.FormValue("thread_ts") != "thread.123" {
							t.Error("fallback must preserve channel and thread")
						}
						if md == deliveryFailureNotice {
							notice = true
						} else if md != "" {
							fmt.Fprint(w, `{"ok":false,"error":"invalid_blocks"}`)
							return
						} else {
							legacyAttempts = append(legacyAttempts, text)
							if rejectLegacy {
								fmt.Fprint(w, `{"ok":false,"error":"invalid_blocks"}`)
								return
							}
							delivered = append(delivered, text)
						}
					case "/chat.delete":
						deleted = true
					}
					fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"stream.1","messages":[]}`)
				}))
				defer srv.Close()
				mgr := &scriptedMgr{events: []agent.ChatEvent{{Type: "text", Delta: body}, {Type: "done"}}}
				bot := newBotWithStream(t, mgr, srv)
				bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

				firstPost := 1
				if rejectUpdate {
					firstPost = 0
				}
				if rejectLegacy {
					if len(legacyAttempts) != 1 || !notice || deleted {
						t.Fatalf("failed fallback: attempts=%d notice=%t deleted=%t", len(legacyAttempts), notice, deleted)
					}
					if len(delivered) != firstPost {
						t.Fatalf("posted past failed chunk: %d delivered", len(delivered))
					}
					return
				}
				want := append([]string(nil), chunks...)
				for i := firstPost; i < len(want); i++ {
					want[i] = PlainToSlack(want[i])
				}
				if !reflect.DeepEqual(delivered, want) || len(legacyAttempts) != len(chunks)-firstPost || notice || deleted != rejectUpdate {
					t.Fatalf("delivery mismatch: chunks=%d/%d attempts=%d notice=%t deleted=%t", len(delivered), len(want), len(legacyAttempts), notice, deleted)
				}
			})
		}
	}
}
