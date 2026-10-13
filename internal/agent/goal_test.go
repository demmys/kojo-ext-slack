package agent

import (
	"strings"
	"testing"
)

func TestParseGoalCommand(t *testing.T) {
	for _, tc := range []struct {
		text, action string
		bad          bool
	}{
		{"!goal fix it", "start", false}, {"!goal\n直して", "start", false}, {"!goal status", "status", false}, {"!goal pause", "pause", false}, {"!goal resume", "resume", false}, {"!goal clear", "clear", false}, {"!goal budget 20000", "budget", false}, {"!goal budget -1", "", true}, {"!goal budget 2 garbage", "", true}, {"!goal " + strings.Repeat("a", 4001), "", true}, {"please !goal fix it", "", false}, {"!goals fix it", "", false}, {"quoted\n!goal fix it", "", false},
	} {
		t.Run(tc.text[:min(len(tc.text), 40)], func(t *testing.T) {
			q, e := ParseGoalCommand(tc.text)
			if (e != nil) != tc.bad {
				t.Fatalf("err=%v", e)
			}
			if tc.bad {
				return
			}
			if tc.action == "" {
				if q != nil {
					t.Fatal(q)
				}
			} else if q == nil || q.Action != tc.action {
				t.Fatalf("got %+v", q)
			}
		})
	}
}
