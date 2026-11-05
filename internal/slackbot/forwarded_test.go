package slackbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

func TestForwardedFilesFromPayload(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "event_callback",
		"event": {
			"type": "message", "channel": "D1", "user": "U1", "ts": "1.0",
			"text": "look",
			"attachments": [
				{"is_share": true, "text": "orig", "files": [
					{"id": "F1", "name": "shot.png", "mimetype": "image/png", "size": 10,
					 "url_private_download": "https://files.slack.com/F1/download"}
				]},
				{"text": "no files"},
				{"files": [{"id": "F2", "name": "b.txt"}]}
			]
		}
	}`)
	got := forwardedFilesFromPayload(raw)
	if len(got) != 2 || got[0].ID != "F1" || got[1].ID != "F2" {
		t.Fatalf("files = %+v", got)
	}
	if got[0].URLPrivateDownload != "https://files.slack.com/F1/download" || got[0].Mimetype != "image/png" {
		t.Fatalf("F1 fields not decoded: %+v", got[0])
	}

	for _, bad := range []json.RawMessage{nil, json.RawMessage(`{`), json.RawMessage(`{"event":{}}`)} {
		if files := forwardedFilesFromPayload(bad); files != nil {
			t.Fatalf("payload %q: want nil, got %+v", bad, files)
		}
	}
}

func TestMergeForwardedFilesSkipsDuplicateIDs(t *testing.T) {
	own := []slack.File{{ID: "F1", Name: "own.png"}}
	fwd := []slack.File{{ID: "F1", Name: "dup.png"}, {ID: "F2"}, {ID: "F2"}, {Name: "noid"}}
	got := mergeForwardedFiles(own, fwd)
	if len(got) != 3 || got[0].Name != "own.png" || got[1].ID != "F2" || got[2].Name != "noid" {
		t.Fatalf("merged = %+v", got)
	}
	if got := mergeForwardedFiles(own, nil); len(got) != 1 {
		t.Fatalf("nil forwarded changed files: %+v", got)
	}
}

func TestResolveFileDownloadURLsUsesFilesInfo(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id := r.Form.Get("file")
		asked = append(asked, r.URL.Path+":"+id)
		w.Header().Set("Content-Type", "application/json")
		if id == "Fmissing" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"file_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"file":{"id":"` + id + `","name":"shot.png","mimetype":"image/png","size":42,"channels":[`+chanFor(id)+`],"url_private_download":"https://files.slack.com/` + id + `/download"}}`))
	}))
	t.Cleanup(srv.Close)
	bot := &Bot{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), logger: testLogger}

	files := bot.resolveFileDownloadURLs(context.Background(), []slack.File{
		{ID: "Fpartial"},
		{ID: "Fready", URLPrivateDownload: "https://already"},
		{ID: "Fmissing"},
		{ID: "Fhidden"},
	}, "U1", "C1")
	if files[0].URLPrivateDownload != "https://files.slack.com/Fpartial/download" ||
		files[0].Name != "shot.png" || files[0].Size != 42 || files[0].Mimetype != "image/png" {
		t.Fatalf("partial file not resolved: %+v", files[0])
	}
	if files[1].URLPrivateDownload != "https://already" {
		t.Fatalf("ready file changed: %+v", files[1])
	}
	if files[2].URLPrivateDownload != "" {
		t.Fatalf("missing file got URL: %+v", files[2])
	}
	if files[3].URLPrivateDownload != "" {
		t.Fatalf("file outside requester's channels got URL: %+v", files[3])
	}
	if len(asked) != 3 {
		t.Fatalf("files.info calls = %v, want partial+missing+hidden", asked)
	}
}

// A forwarded message has no top-level files; its image lives in
// attachments[].files. It must be downloaded like a direct attachment.
func TestForwardedMessageFileIsDownloaded(t *testing.T) {
	withTempUploadDir(t)
	downloadStarted := make(chan struct{})
	var fileServer *httptest.Server
	fileServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"file":{"id":"F9","name":"shot.png","size":3,"channels":["C1"],"url_private_download":"` + fileServer.URL + `/dl"}}`))
		case "/dl":
			close(downloadStarted)
			_, _ = w.Write([]byte("png"))
		default:
			t.Errorf("unexpected request %s (payload URL must not be fetched)", r.URL.Path)
		}
	}))
	t.Cleanup(fileServer.Close)

	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.botToken = "xoxb-test"
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(fileServer.URL+"/"))
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", cancel)
	forwarded := forwardedMessagesFromPayload(json.RawMessage(`{"event":{"attachments":[{"is_share":true,"files":[
		{"id":"F9","name":"shot.png","mimetype":"image/png","size":3,"url_private_download":"` + fileServer.URL + `/attacker"}]}]}}`))
	bot.handleMessageEventWithForwarded(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.1", TimeStamp: "100.000002", Text: "what is this",
	}, forwarded)
	select {
	case <-downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("forwarded attachment download did not start")
	}
	cancel()
	bot.finishActiveTurn("C1", "thread.1", source)
	bot.releaseThreadReservation("C1", "thread.1", hold)
}

func TestForwardedMessagesFromPayloadKeepsContext(t *testing.T) {
	raw := json.RawMessage(`{"event":{"attachments":[
		{"title":"unfurl","text":"site preview","from_url":"https://example.com"},
		{"is_share":true,"author_id":"U9","channel_id":"C42","channel_name":"dev-eng","ts":"1792483000.000100",
		 "from_url":"https://x.slack.com/archives/C42/p1792483000000100","text":"look <@U9>",
		 "files":[{"id":"F1","name":"shot.png"}]}]}}`)
	msgs := forwardedMessagesFromPayload(raw)
	if len(msgs) != 1 {
		t.Fatalf("want 1 forward (unfurl skipped), got %d", len(msgs))
	}
	got := appendForwardedMessages("この画像見える?", msgs)
	for _, want := range []string{
		"この画像見える?\n\n[Forwarded message]\n",
		"from: #dev-eng (C42) by <@U9> at ",
		"link: https://x.slack.com/archives/C42/p1792483000000100\n",
		"text:\nlook <@U9>\n",
		"files: shot.png\n",
		"[/Forwarded message]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if files := forwardedFiles(msgs); len(files) != 1 || files[0].ID != "F1" {
		t.Fatalf("files = %+v", files)
	}
}

func TestAppendForwardedMessagesTextOnlyAndFallbacks(t *testing.T) {
	got := appendForwardedMessages("", []forwardedMessage{{IsShare: true, ChannelID: "C1", AuthorName: "Bob", Fallback: "fb"}})
	want := "[Forwarded message]\nfrom: C1 by Bob\ntext:\nfb\n[/Forwarded message]"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestForwardedMessagesNumericTS(t *testing.T) {
	msgs := forwardedMessagesFromPayload(json.RawMessage(`{"event":{"attachments":[{"is_share":true,"ts":1792483000,"text":"hi"}]}}`))
	if len(msgs) != 1 || msgs[0].TS != "1792483000" {
		t.Fatalf("got %+v", msgs)
	}
}

func chanFor(id string) string {
	if id == "Fhidden" {
		return ""
	}
	return `"C1"`
}

func TestAllowedSlackFileURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://files.slack.com/files-pri/T1-F1/download/a.png": true,
		"http://files.slack.com/x":                               false,
		"https://attacker.example/x":                             false,
		"https://files.slack.com.attacker.example/x":             false,
		"https://u:p@files.slack.com/x":                          false,
		"":                                                       false,
	} {
		if got := realAllowedSlackFileURL(raw); got != want {
			t.Errorf("%q: got %v want %v", raw, got, want)
		}
	}
}

// Tests serve downloads from httptest (http://127.0.0.1); production keeps
// the Slack-only check, exercised directly by TestAllowedSlackFileURL.
var realAllowedSlackFileURL = allowedSlackFileURL

func init() {
	allowedSlackFileURL = func(raw string) bool {
		return strings.HasPrefix(raw, "http://127.0.0.1:") || realAllowedSlackFileURL(raw)
	}
}
