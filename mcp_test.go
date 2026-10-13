package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type failingUploadReader struct{}

func (failingUploadReader) Read([]byte) (int, error) {
	return 0, errors.New("peer stream interrupted")
}

func TestPostSlackMultipartFileReturnsWhenSourceFails(t *testing.T) {
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upload.Close)

	done := make(chan error, 1)
	go func() {
		done <- postSlackMultipartFile(
			context.Background(), upload.Client(), upload.URL, "test-token", "result.txt", failingUploadReader{},
		)
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "peer stream interrupted") {
			t.Fatalf("error = %v, want interrupted source error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("multipart upload hung after the source stream failed")
	}
}

func TestPostSlackMultipartFileStreamsFile(t *testing.T) {
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		raw, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
		}
		if header.Filename != "result.txt" || string(raw) != "holder file" {
			t.Errorf("filename=%q body=%q", header.Filename, raw)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upload.Close)

	if err := postSlackMultipartFile(
		context.Background(), upload.Client(), upload.URL, "test-token", "result.txt", strings.NewReader("holder file"),
	); err != nil {
		t.Fatal(err)
	}
}

func TestParseListChannelsArgs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want listChannelsArgs
	}{
		{
			name: "defaults when no args",
			in:   map[string]any{},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
		{
			name: "explicit limit honored",
			in:   map[string]any{"limit": float64(50)},
			want: listChannelsArgs{Limit: 50, MemberOnly: true},
		},
		{
			name: "limit clamped to max",
			in:   map[string]any{"limit": float64(10_000)},
			want: listChannelsArgs{Limit: listChannelsMaxLimit, MemberOnly: true},
		},
		{
			name: "limit beyond the int range clamped to max",
			in:   map[string]any{"limit": 1e300},
			want: listChannelsArgs{Limit: listChannelsMaxLimit, MemberOnly: true},
		},
		{
			name: "non-positive limit falls back to default",
			in:   map[string]any{"limit": float64(0)},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
		{
			name: "name_contains is lowercased",
			in:   map[string]any{"name_contains": "General"},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, NameFilter: "general", MemberOnly: true},
		},
		{
			name: "member_only false respected",
			in:   map[string]any{"member_only": false},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: false},
		},
		{
			name: "wrong types ignored",
			in:   map[string]any{"limit": "200", "member_only": "yes", "name_contains": 3},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseListChannelsArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseListChannelsArgs(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMatchChannel(t *testing.T) {
	mkCh := func(id, name string, member bool) slack.Channel {
		var ch slack.Channel
		ch.ID = id
		ch.Name = name
		ch.IsMember = member
		ch.Topic.Value = "topic-" + name
		ch.Purpose.Value = "purpose-" + name
		ch.NumMembers = 3
		return ch
	}

	t.Run("member_only filters out non-members", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "general", false), listChannelsArgs{MemberOnly: true, Limit: 10})
		if ok {
			t.Fatal("expected non-member channel to be filtered out")
		}
	})

	t.Run("member_only=false keeps non-members", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "general", false), listChannelsArgs{MemberOnly: false, Limit: 10})
		if !ok {
			t.Fatal("expected non-member channel to pass when memberOnly=false")
		}
	})

	t.Run("name filter is case-insensitive substring match", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "Engineering", true),
			listChannelsArgs{NameFilter: "engine", MemberOnly: true, Limit: 10})
		if !ok {
			t.Fatal("expected engineering to match 'engine'")
		}
		_, ok = matchChannel(mkCh("C1", "sales", true),
			listChannelsArgs{NameFilter: "engine", MemberOnly: true, Limit: 10})
		if ok {
			t.Fatal("expected sales not to match 'engine'")
		}
	})

	t.Run("info carries all metadata", func(t *testing.T) {
		info, ok := matchChannel(mkCh("C1", "general", true),
			listChannelsArgs{MemberOnly: true, Limit: 10})
		if !ok {
			t.Fatal("expected match")
		}
		want := channelInfo{
			ID: "C1", Name: "general",
			Topic: "topic-general", Purpose: "purpose-general",
			NumMembers: 3, IsMember: true,
		}
		if info != want {
			t.Errorf("info = %+v, want %+v", info, want)
		}
	})
}

// fakeLister implements slackConversationLister using pre-programmed pages.
type fakeLister struct {
	pages [][]slack.Channel
	next  []string // next cursor per page (same length as pages)
	errAt int      // 1-indexed; 0 = never error
	calls int
}

func (f *fakeLister) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	f.calls++
	if f.errAt > 0 && f.calls == f.errAt {
		return nil, "", errors.New("boom")
	}
	if f.calls > len(f.pages) {
		return nil, "", nil
	}
	idx := f.calls - 1
	return f.pages[idx], f.next[idx], nil
}

func mkCh(id, name string, member bool) slack.Channel {
	var ch slack.Channel
	ch.ID = id
	ch.Name = name
	ch.IsMember = member
	return ch
}

func TestListSlackChannelsStopsAtLimit(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{
			{mkCh("C1", "a", true), mkCh("C2", "b", true), mkCh("C3", "c", true)},
		},
		next: []string{""},
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 2, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(got))
	}
}

func TestListSlackChannelsPaginates(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{
			{mkCh("C1", "a", true)},
			{mkCh("C2", "b", true)},
		},
		next: []string{"cursor2", ""},
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fl.calls != 2 {
		t.Errorf("expected 2 API calls (pagination), got %d", fl.calls)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 channels, got %d", len(got))
	}
}

func TestListSlackChannelsMaxPagesCap(t *testing.T) {
	// Every page returns a new cursor, so without the cap we'd loop forever.
	pages := make([][]slack.Channel, 10)
	nexts := make([]string, 10)
	for i := range pages {
		pages[i] = []slack.Channel{mkCh("C", "ch", true)}
		nexts[i] = "more"
	}
	fl := &fakeLister{pages: pages, next: nexts}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 1000, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fl.calls != listChannelsMaxPages {
		t.Errorf("expected %d calls (max pages), got %d", listChannelsMaxPages, fl.calls)
	}
	if len(got) != listChannelsMaxPages {
		t.Errorf("expected %d channels, got %d", listChannelsMaxPages, len(got))
	}
}

func TestListSlackChannelsPartialFailureReturnsCollected(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{{mkCh("C1", "a", true)}, nil},
		next:  []string{"cursor2", ""},
		errAt: 2,
	}
	var partialErr error
	var partialCount int
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true},
		func(e error, c int) { partialErr = e; partialCount = c },
	)
	if err != nil {
		t.Fatalf("expected nil err on partial success, got %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 channel, got %d", len(got))
	}
	if partialErr == nil || partialCount != 1 {
		t.Errorf("onPartial not invoked correctly: err=%v count=%d", partialErr, partialCount)
	}
}

func TestListSlackChannelsFirstPageErrorReturnsError(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{nil},
		next:  []string{""},
		errAt: 1,
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true}, nil)
	if err == nil {
		t.Fatal("expected error on first-page failure")
	}
	if got != nil {
		t.Errorf("expected nil channels on hard error, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// clampLimit (shared by history, thread replies, and list_users handlers)
// ---------------------------------------------------------------------------

func TestClampLimit(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		def  int
		max  int
		want int
	}{
		{"missing arg uses default", nil, 20, 100, 20},
		{"wrong type uses default", "50", 20, 100, 20},
		{"zero uses default", float64(0), 20, 100, 20},
		{"negative uses default", float64(-5), 20, 100, 20},
		{"fractional below 1 uses default", float64(0.5), 20, 100, 20},
		{"explicit honored", float64(50), 20, 100, 50},
		{"explicit one honored", float64(1), 20, 100, 1},
		{"explicit fractional rounds down to int", float64(50.7), 20, 100, 50},
		{"over max clamps", float64(999), 20, 100, 100},
		{"history defaults", nil, historyDefaultLimit, historyMaxLimit, historyDefaultLimit},
		{"history over max", float64(999), historyDefaultLimit, historyMaxLimit, historyMaxLimit},
		{"thread defaults", nil, threadRepliesDefaultLimit, threadRepliesMaxLimit, threadRepliesDefaultLimit},
		{"thread over max", float64(9999), threadRepliesDefaultLimit, threadRepliesMaxLimit, threadRepliesMaxLimit},
		{"users defaults", nil, listUsersDefaultLimit, listUsersMaxLimit, listUsersDefaultLimit},
		{"users over max", float64(9999), listUsersDefaultLimit, listUsersMaxLimit, listUsersMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLimit(tc.raw, tc.def, tc.max); got != tc.want {
				t.Errorf("clampLimit(%v, %d, %d) = %d, want %d", tc.raw, tc.def, tc.max, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// filterEmoji
// ---------------------------------------------------------------------------

func TestFilterEmoji(t *testing.T) {
	emojiMap := map[string]string{
		"partyparrot": "https://example.com/partyparrot.gif",
		"shipit":      "alias:squirrel",
		"thumbsup2":   "https://example.com/thumbsup2.png",
		"PartyHat":    "https://example.com/partyhat.gif",
	}

	t.Run("substring match (case-insensitive)", func(t *testing.T) {
		got := filterEmoji(emojiMap, "PARTY")
		if len(got) != 2 {
			t.Fatalf("expected 2 emoji matching 'PARTY', got %d (%+v)", len(got), got)
		}
		names := map[string]bool{}
		for _, e := range got {
			names[e.Name] = true
		}
		if !names["partyparrot"] || !names["PartyHat"] {
			t.Errorf("expected partyparrot+PartyHat, got %+v", got)
		}
	})

	t.Run("empty filter returns all", func(t *testing.T) {
		got := filterEmoji(emojiMap, "")
		if len(got) != len(emojiMap) {
			t.Errorf("expected %d emoji, got %d", len(emojiMap), len(got))
		}
	})

	t.Run("no match returns nil", func(t *testing.T) {
		got := filterEmoji(emojiMap, "nonexistent")
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("preserves value (URL or alias)", func(t *testing.T) {
		got := filterEmoji(emojiMap, "shipit")
		if len(got) != 1 || got[0].Value != "alias:squirrel" {
			t.Errorf("expected alias:squirrel, got %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// matchUser
// ---------------------------------------------------------------------------

func TestMatchUser(t *testing.T) {
	mkUser := func(id, name, realName, displayName string, isBot, deleted bool) slack.User {
		var u slack.User
		u.ID = id
		u.Name = name
		u.RealName = realName
		u.Profile.DisplayName = displayName
		u.IsBot = isBot
		u.Deleted = deleted
		return u
	}

	alice := mkUser("U1", "alice", "Alice Smith", "Alice", false, false)
	bob := mkUser("U2", "bob", "Bob Jones", "Bobby", false, false)
	slackbot := mkUser("USLACKBOT", "slackbot", "Slackbot", "", false, false)
	gone := mkUser("U4", "alice_zombie", "Gone Alice", "", false, true)
	aliceBot := mkUser("U5", "alice_bot", "Alice Bot", "AliceBot", true, false)

	t.Run("deleted always excluded", func(t *testing.T) {
		if _, ok := matchUser(gone, "alice", true); ok {
			t.Error("deleted user should be excluded even with includeBots=true and matching name")
		}
	})

	t.Run("bots excluded by default", func(t *testing.T) {
		if _, ok := matchUser(aliceBot, "", false); ok {
			t.Error("bot user should be excluded when includeBots=false")
		}
		if _, ok := matchUser(slackbot, "", false); ok {
			t.Error("USLACKBOT should be excluded by ID even when IsBot=false")
		}
	})

	t.Run("bots included when requested", func(t *testing.T) {
		if _, ok := matchUser(aliceBot, "", true); !ok {
			t.Error("bot user should pass when includeBots=true")
		}
	})

	t.Run("name filter matches Name/RealName/DisplayName", func(t *testing.T) {
		if _, ok := matchUser(alice, "alice", false); !ok {
			t.Error("alice should match by Name")
		}
		if _, ok := matchUser(bob, "jones", false); !ok {
			t.Error("bob should match by RealName 'Bob Jones'")
		}
		if _, ok := matchUser(bob, "bobby", false); !ok {
			t.Error("bob should match by DisplayName 'Bobby'")
		}
		if _, ok := matchUser(bob, "alice", false); ok {
			t.Error("bob should not match 'alice'")
		}
	})

	t.Run("name filter is case-insensitive without caller lowercasing", func(t *testing.T) {
		// matchUser must lowercase the needle internally so callers can
		// pass the user's raw input.
		if _, ok := matchUser(alice, "ALICE", false); !ok {
			t.Error("alice should match 'ALICE' (helper must lowercase)")
		}
		if _, ok := matchUser(bob, "JoNeS", false); !ok {
			t.Error("bob should match 'JoNeS'")
		}
	})

	t.Run("info populated correctly", func(t *testing.T) {
		got, ok := matchUser(alice, "", false)
		if !ok {
			t.Fatal("expected match")
		}
		want := userInfo{
			ID: "U1", Name: "alice", RealName: "Alice Smith", DisplayName: "Alice",
		}
		if got != want {
			t.Errorf("info = %+v, want %+v", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// openUploadPath
// ---------------------------------------------------------------------------

func TestOpenUploadPath(t *testing.T) {
	// Use a per-test directory as the upload directory.
	tmpRoot := t.TempDir()
	uploadRoot := filepath.Join(tmpRoot, "kojo-upload")
	if err := os.MkdirAll(uploadRoot, 0o755); err != nil {
		t.Fatalf("mkdir upload root: %v", err)
	}

	t.Run("regular file inside upload dir is opened", func(t *testing.T) {
		p := filepath.Join(uploadRoot, "ok.txt")
		if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, size, kind := openUploadPath(uploadRoot, p)
		if kind != "" {
			t.Fatalf("kind = %q, want empty", kind)
		}
		if f == nil {
			t.Fatal("expected non-nil file")
		}
		t.Cleanup(func() { f.Close() })
		if size != 5 {
			t.Errorf("size = %d, want 5", size)
		}
		// Read through the fd to confirm the data really comes from the
		// validated inode.
		buf := make([]byte, 5)
		if _, err := f.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != "hello" {
			t.Errorf("data = %q, want %q", buf, "hello")
		}
	})

	t.Run("empty path rejected", func(t *testing.T) {
		f, _, kind := openUploadPath(uploadRoot, "")
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrEmpty {
			t.Errorf("kind = %q, want %q", kind, uploadErrEmpty)
		}
	})

	t.Run("path outside upload dir rejected", func(t *testing.T) {
		outside := filepath.Join(tmpRoot, "outside.txt")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(uploadRoot, outside)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		dir := filepath.Join(uploadRoot, "subdir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(uploadRoot, dir)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrIsDir {
			t.Errorf("kind = %q, want %q", kind, uploadErrIsDir)
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		f, _, kind := openUploadPath(uploadRoot, filepath.Join(uploadRoot, "does-not-exist"))
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrNotFound {
			t.Errorf("kind = %q, want %q", kind, uploadErrNotFound)
		}
	})

	t.Run("symlink to outside upload dir rejected", func(t *testing.T) {
		secret := filepath.Join(tmpRoot, "secret.txt")
		if err := os.WriteFile(secret, []byte("s3cr3t"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(uploadRoot, "evil-link")
		if err := os.Symlink(secret, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		f, _, kind := openUploadPath(uploadRoot, link)
		if f != nil {
			f.Close()
			t.Error("symlink escape: expected rejection but got opened file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("user message never includes path", func(t *testing.T) {
		// Sanity check: the user-facing strings are fixed and contain
		// no path interpolation.
		for _, kind := range []string{
			uploadErrEmpty, uploadErrInvalid, uploadErrNotFound,
			uploadErrOutside, uploadErrIsDir, uploadErrNotFile,
			uploadErrOpenFail, uploadErrStatFail, uploadErrSwapped,
		} {
			msg := uploadPathUserMessage(kind)
			if strings.Contains(msg, "/") || strings.Contains(msg, `\`) {
				t.Errorf("kind %q produced message containing path separator: %q", kind, msg)
			}
			if msg == "" {
				t.Errorf("kind %q produced empty message", kind)
			}
		}
	})
}

func TestToMessageInfo(t *testing.T) {
	t.Run("minimal user message preserves user/text keys", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U123",
			Text:      "hello",
			Timestamp: "1700000000.000100",
		}, nil)
		if got.User != "U123" || got.Text != "hello" || got.Timestamp != "1700000000.000100" {
			t.Fatalf("basic fields lost: %+v", got)
		}
		// Confirm JSON shape: "user" and "text" must always be present
		// (legacy consumers may rely on these keys existing) while richer
		// optional fields stay omitted.
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, mustHave := range []string{`"user":"U123"`, `"text":"hello"`, `"timestamp":"1700000000.000100"`} {
			if !strings.Contains(s, mustHave) {
				t.Errorf("expected %q in %s", mustHave, s)
			}
		}
		for _, mustNot := range []string{`"files"`, `"reactions"`, `"attachments"`, `"blocks"`, `"metadata"`, `"edited"`, `"bot_id"`, `"username"`, `"app_id"`} {
			if strings.Contains(s, mustNot) {
				t.Errorf("did not expect %q in %s", mustNot, s)
			}
		}
	})

	t.Run("bot message exposes bot_id/username/app_id", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			Type:      "message",
			SubType:   "bot_message",
			BotID:     "B123",
			Username:  "ReleaseBot",
			Text:      "",
			Timestamp: "1700000001.000200",
			BotProfile: &slack.BotProfile{
				AppID: "A999",
			},
		}, nil)
		if got.BotID != "B123" || got.Username != "ReleaseBot" || got.AppID != "A999" {
			t.Fatalf("bot fields lost: %+v", got)
		}
		if got.SubType != "bot_message" {
			t.Errorf("subtype lost")
		}
		// User key still emitted as empty string for compatibility.
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), `"user":""`) {
			t.Errorf("expected empty user key in %s", raw)
		}
	})

	t.Run("files attachments reactions edited preserved", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "see file",
			Timestamp: "1700000002.000300",
			Edited:    &slack.Edited{User: "U1", Timestamp: "1700000003.000400"},
			Files: []slack.File{{
				ID:                 "F1",
				Name:               "report.pdf",
				Title:              "Q4 report",
				Mimetype:           "application/pdf",
				Filetype:           "pdf",
				PrettyType:         "PDF",
				Size:               4096,
				URLPrivate:         "https://files.slack.com/private/F1",
				URLPrivateDownload: "https://files.slack.com/private/F1/download",
				Permalink:          "https://workspace.slack.com/files/U1/F1",
				PermalinkPublic:    "https://slack-files.com/pub/F1",
				User:               "U1",
			}},
			Reactions: []slack.ItemReaction{{
				Name:  "thumbsup",
				Count: 2,
				Users: []string{"U2", "U3"},
			}},
			Attachments: []slack.Attachment{{
				Color:       "good",
				Fallback:    "fb",
				Title:       "Build #42",
				TitleLink:   "https://ci.example.com/42",
				Pretext:     "ok",
				Text:        "passed",
				AuthorName:  "ci",
				AuthorLink:  "https://ci.example.com",
				ServiceName: "CI",
				FromURL:     "https://ci.example.com/42",
				ImageURL:    "https://ci.example.com/42.png",
				ThumbURL:    "https://ci.example.com/42-thumb.png",
				Footer:      "ci-bot",
				Fields: []slack.AttachmentField{
					{Title: "branch", Value: "main", Short: true},
				},
			}},
		}, nil)
		if got.Edited == nil || got.Edited.User != "U1" || got.Edited.Timestamp != "1700000003.000400" {
			t.Errorf("edited info lost: %+v", got.Edited)
		}
		if len(got.Files) != 1 || got.Files[0].ID != "F1" || got.Files[0].URLPrivate == "" || got.Files[0].Permalink == "" {
			t.Errorf("files lost: %+v", got.Files)
		}
		if len(got.Reactions) != 1 || got.Reactions[0].Name != "thumbsup" || got.Reactions[0].Count != 2 || !reflect.DeepEqual(got.Reactions[0].Users, []string{"U2", "U3"}) {
			t.Errorf("reactions lost: %+v", got.Reactions)
		}
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments lost: %+v", got.Attachments)
		}
		att := got.Attachments[0]
		if att.Title != "Build #42" || att.Color != "good" || att.Footer != "ci-bot" || att.ImageURL == "" {
			t.Errorf("attachment scalar fields lost: %+v", att)
		}
		if len(att.Fields) != 1 || att.Fields[0].Title != "branch" || att.Fields[0].Value != "main" || !att.Fields[0].Short {
			t.Errorf("attachment fields lost: %+v", att.Fields)
		}
	})

	t.Run("block kit blocks are re-encoded as JSON", func(t *testing.T) {
		section := slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*hello*", false, false),
			nil, nil,
		)
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "fallback text",
			Timestamp: "1700000004.000500",
			Blocks:    slack.Blocks{BlockSet: []slack.Block{section}},
		}, nil)
		if len(got.Blocks) == 0 {
			t.Fatalf("blocks dropped")
		}
		var decoded []map[string]any
		if err := json.Unmarshal(got.Blocks, &decoded); err != nil {
			t.Fatalf("blocks not valid JSON: %v (%s)", err, got.Blocks)
		}
		if len(decoded) != 1 || decoded[0]["type"] != "section" {
			t.Fatalf("expected single section block, got %s", got.Blocks)
		}
		raw := string(got.Blocks)
		if !strings.Contains(raw, `*hello*`) {
			t.Errorf("section block content missing: %s", raw)
		}
	})

	t.Run("metadata passed through when present", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "evt",
			Timestamp: "1700000005.000600",
			Metadata: slack.SlackMetadata{
				EventType:    "deploy_finished",
				EventPayload: map[string]any{"service": "kojo", "ok": true},
			},
		}, nil)
		if got.Metadata == nil || got.Metadata.EventType != "deploy_finished" {
			t.Fatalf("metadata lost: %+v", got.Metadata)
		}
		if v, ok := got.Metadata.EventPayload["service"].(string); !ok || v != "kojo" {
			t.Errorf("event payload lost: %+v", got.Metadata.EventPayload)
		}
	})

	t.Run("absent metadata stays nil", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "no meta",
			Timestamp: "1700000006.000700",
		}, nil)
		if got.Metadata != nil {
			t.Errorf("expected nil metadata, got %+v", got.Metadata)
		}
	})
}

// ---------------------------------------------------------------------------
// runMCP over in-memory pipes against a fake Slack Web API
// ---------------------------------------------------------------------------

const (
	testBotToken  = "xoxb-test"
	testChannelID = "C0123456789"
	testFileID    = "F0123456789"

	// channelsReply is a conversations.list page with one joined and one
	// unjoined channel.
	channelsReply = `{"ok":true,"channels":[` +
		`{"id":"C0123456789","name":"general","is_member":true,"num_members":3,"topic":{"value":"chat"},"purpose":{"value":"everything"}},` +
		`{"id":"C0987654321","name":"random","is_member":false,"num_members":7}` +
		`],"response_metadata":{"next_cursor":""}}`
)

type fakeUpload struct {
	auth     string
	filename string
	body     string
}

// fakeSlack is an httptest stand-in for the Slack Web API. Each method
// answers with the body registered through reply; requests are recorded so
// tests can check what the tools sent.
type fakeSlack struct {
	srv *httptest.Server

	mu           sync.Mutex
	replies      map[string]string
	forms        map[string][]url.Values
	uploads      []fakeUpload
	uploadStatus int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		replies:      map[string]string{},
		forms:        map[string][]url.Values{},
		uploadStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		if err := r.ParseForm(); err != nil {
			t.Errorf("%s: ParseForm: %v", method, err)
		}
		f.mu.Lock()
		f.forms[method] = append(f.forms[method], r.Form)
		body, ok := f.replies[method]
		f.mu.Unlock()
		if !ok {
			body = `{"ok":false,"error":"unknown_method"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		up := fakeUpload{auth: r.Header.Get("Authorization")}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upload: ParseMultipartForm: %v", err)
		} else if file, header, err := r.FormFile("file"); err != nil {
			t.Errorf("upload: FormFile: %v", err)
		} else {
			raw, _ := io.ReadAll(file)
			_ = file.Close()
			up.filename, up.body = header.Filename, string(raw)
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, up)
		status := f.uploadStatus
		f.mu.Unlock()
		w.WriteHeader(status)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) reply(method, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = body
}

// lastForm returns the parameters of the most recent call to method.
func (f *fakeSlack) lastForm(t *testing.T, method string) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.forms[method]
	if len(calls) == 0 {
		t.Fatalf("%s was never called", method)
	}
	return calls[len(calls)-1]
}

func (f *fakeSlack) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms[method])
}

func (f *fakeSlack) options() mcpOptions {
	return mcpOptions{
		BotToken:     func(context.Context) (string, error) { return testBotToken, nil },
		SlackOptions: []slack.Option{slack.OptionAPIURL(f.srv.URL + "/api/")},
	}
}

// mcpSession drives one runMCP instance through a pair of in-memory pipes.
type mcpSession struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan string
	id    int
}

func startMCP(t *testing.T, opts mcpOptions) *mcpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	m := &mcpSession{t: t, in: inW, lines: make(chan string, 16)}
	done := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		err := runMCP(context.Background(), inR, outW, logger, "1.2.3", opts)
		_ = outW.Close()
		done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			m.lines <- sc.Text()
		}
		close(m.lines)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runMCP: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runMCP did not return after its input was closed")
		}
	})
	return m
}

func (m *mcpSession) send(line string) {
	m.t.Helper()
	if _, err := io.WriteString(m.in, line+"\n"); err != nil {
		m.t.Fatalf("write request: %v", err)
	}
}

func (m *mcpSession) recv() map[string]any {
	m.t.Helper()
	select {
	case line, ok := <-m.lines:
		if !ok {
			m.t.Fatal("server closed its output")
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			m.t.Fatalf("response is not JSON: %v (%s)", err, line)
		}
		if resp["jsonrpc"] != "2.0" {
			m.t.Errorf("jsonrpc = %v, want 2.0 (%s)", resp["jsonrpc"], line)
		}
		return resp
	case <-time.After(10 * time.Second):
		m.t.Fatal("timed out waiting for a response")
	}
	return nil
}

// request sends one request and returns the response carrying its id.
func (m *mcpSession) request(method string, params any) map[string]any {
	m.t.Helper()
	m.id++
	req := map[string]any{"jsonrpc": "2.0", "id": m.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		m.t.Fatal(err)
	}
	m.send(string(raw))
	resp := m.recv()
	if got, ok := resp["id"].(float64); !ok || int(got) != m.id {
		m.t.Fatalf("response id = %v, want %d", resp["id"], m.id)
	}
	return resp
}

// result is request for calls that must not fail at the protocol level.
func (m *mcpSession) result(method string, params any) map[string]any {
	m.t.Helper()
	resp := m.request(method, params)
	if e, ok := resp["error"]; ok {
		m.t.Fatalf("%s returned a JSON-RPC error: %v", method, e)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		m.t.Fatalf("%s result is not an object: %v", method, resp["result"])
	}
	return res
}

// callTool invokes a tool and returns the text of its result and whether
// the result was flagged as an error.
func (m *mcpSession) callTool(name string, args map[string]any) (string, bool) {
	m.t.Helper()
	res := m.result("tools/call", map[string]any{"name": name, "arguments": args})
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		m.t.Fatalf("%s: content = %v, want one item", name, res["content"])
	}
	item, _ := content[0].(map[string]any)
	if item["type"] != "text" {
		m.t.Fatalf("%s: content type = %v, want text", name, item["type"])
	}
	text, _ := item["text"].(string)
	isError, present := res["isError"]
	if present && isError != true {
		m.t.Errorf("%s: isError = %v; it must be true or absent", name, isError)
	}
	return text, present
}

func (m *mcpSession) wantText(name string, args map[string]any, want string) {
	m.t.Helper()
	text, isError := m.callTool(name, args)
	if isError {
		m.t.Fatalf("%s failed: %s", name, text)
	}
	if text != want {
		m.t.Errorf("%s result =\n%s\nwant\n%s", name, text, want)
	}
}

func (m *mcpSession) wantError(name string, args map[string]any, want string) {
	m.t.Helper()
	text, isError := m.callTool(name, args)
	if !isError {
		m.t.Fatalf("%s succeeded (%s), want error %q", name, text, want)
	}
	if text != want {
		m.t.Errorf("%s error = %q, want %q", name, text, want)
	}
}

func wantRPCError(t *testing.T, resp map[string]any, code int, message string) {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", resp)
	}
	if got, _ := e["code"].(float64); int(got) != code {
		t.Errorf("error code = %v, want %d", e["code"], code)
	}
	if e["message"] != message {
		t.Errorf("error message = %v, want %q", e["message"], message)
	}
}

func TestMCPInitialize(t *testing.T) {
	m := startMCP(t, mcpOptions{})

	res := m.result("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's supported revision", res["protocolVersion"])
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "slack" || info["version"] != "1.2.3" {
		t.Errorf("serverInfo = %v, want name slack and version 1.2.3", info)
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any); !ok {
		t.Errorf("capabilities = %v, want a tools capability", caps)
	}

	res = m.result("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if res["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s for an unknown revision", res["protocolVersion"], mcpProtocolVersion)
	}

	if res := m.result("ping", nil); len(res) != 0 {
		t.Errorf("ping result = %v, want empty object", res)
	}
}

// builtinToolsJSON is the "tools" array kojo's built-in MCP server returned
// from tools/list, captured from its tool definitions (one tool per line,
// in the order it listed them). The port must advertise exactly this.
const builtinToolsJSON = `[
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Add an emoji reaction to a message. The emoji name should NOT include colons (e.g. use 'thumbsup' not ':thumbsup:').","inputSchema":{"properties":{"channel":{"description":"Channel ID where the message is","type":"string"},"emoji":{"description":"Emoji name without colons (e.g. 'thumbsup', 'heart', 'eyes')","type":"string"},"timestamp":{"description":"Timestamp of the message to react to","type":"string"}},"required":["channel","timestamp","emoji"],"type":"object"},"name":"slack_add_reaction"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Get recent messages from a Slack channel. Returns messages in reverse chronological order (newest first). Each message includes text, author (user/bot/app), timestamp, thread_ts, subtype, edited info, files (id/name/mimetype/size/permalink/url_private), reactions, attachments (title/text/fields/image_url/footer), Block Kit blocks (re-encoded JSON; unknown block types may be lossy), and metadata.","inputSchema":{"properties":{"channel":{"description":"Channel ID or #channel-name","type":"string"},"latest":{"description":"Only messages before this Unix timestamp","type":"string"},"limit":{"description":"Number of messages to retrieve (default 20, max 100)","type":"number"},"oldest":{"description":"Only messages after this Unix timestamp","type":"string"}},"required":["channel"],"type":"object"},"name":"slack_get_channel_history"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Get all replies in a message thread. Each message includes text, author (user/bot/app), timestamp, edited info, files (id/name/mimetype/size/permalink/url_private), reactions, attachments (title/text/fields/image_url/footer), Block Kit blocks (re-encoded JSON; unknown block types may be lossy), and metadata.","inputSchema":{"properties":{"channel":{"description":"Channel ID where the thread exists","type":"string"},"limit":{"description":"Maximum number of replies to return (default 50, max 200)","type":"number"},"thread_ts":{"description":"Timestamp of the parent message (thread root)","type":"string"}},"required":["channel","thread_ts"],"type":"object"},"name":"slack_get_thread_replies"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"List Slack channels the bot can access. Returns channel ID, name, topic, and member count. By default returns only channels the bot has joined. Use member_only=false to include all visible channels (may be slow for large workspaces).","inputSchema":{"properties":{"limit":{"description":"Maximum number of channels to return (default 200, max 1000)","type":"number"},"member_only":{"description":"If true (default), only return channels the bot has joined","type":"boolean"},"name_contains":{"description":"Filter channels whose name contains this substring (case-insensitive)","type":"string"}},"required":[],"type":"object"},"name":"slack_list_channels"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"List custom emoji available in the Slack workspace. Returns emoji names and their image URLs or aliases. Standard Unicode emoji (like :thumbsup:) are always available and not included in this list.","inputSchema":{"properties":{"name_contains":{"description":"Filter emoji whose name contains this substring (case-insensitive)","type":"string"}},"required":[],"type":"object"},"name":"slack_list_emoji"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"List users in the Slack workspace. Returns user ID, name, display name, real name, and bot/admin flags. Useful for resolving @mentions or finding user IDs.","inputSchema":{"properties":{"include_bots":{"description":"If true, include bot users in the result (default false)","type":"boolean"},"limit":{"description":"Maximum number of users to return (default 200, max 500)","type":"number"},"name_contains":{"description":"Filter users whose name or display name contains this substring (case-insensitive)","type":"string"}},"required":[],"type":"object"},"name":"slack_list_users"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Post a message to a Slack channel. The channel parameter can be a channel ID (C0123ABC) or a #channel-name.","inputSchema":{"properties":{"channel":{"description":"Channel ID or #channel-name to post to","type":"string"},"text":{"description":"Message text (supports Slack mrkdwn formatting)","type":"string"}},"required":["channel","text"],"type":"object"},"name":"slack_post_message"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Remove an emoji reaction from a message.","inputSchema":{"properties":{"channel":{"description":"Channel ID where the message is","type":"string"},"emoji":{"description":"Emoji name without colons (e.g. 'thumbsup')","type":"string"},"timestamp":{"description":"Timestamp of the message","type":"string"}},"required":["channel","timestamp","emoji"],"type":"object"},"name":"slack_remove_reaction"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Post a reply to a specific thread in a Slack channel. Use this when you want to reply in a thread rather than posting a new top-level message.","inputSchema":{"properties":{"broadcast":{"description":"If true, also post the reply to the channel (default false)","type":"boolean"},"channel":{"description":"Channel ID or #channel-name where the thread exists","type":"string"},"text":{"description":"Reply text (supports Slack mrkdwn formatting)","type":"string"},"thread_ts":{"description":"Timestamp of the parent message (thread root)","type":"string"}},"required":["channel","thread_ts","text"],"type":"object"},"name":"slack_reply_to_thread"},
{"annotations":{"destructiveHint":true,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"Upload a file to a Slack channel. Provide file_path (preferred for binary/large files) or content (for text snippets). Only one source allowed.","inputSchema":{"properties":{"channel":{"description":"Channel ID or #channel-name to upload to","type":"string"},"content":{"description":"Plain text content for the file (for text/code snippets). Use this OR file_path.","type":"string"},"file_path":{"description":"Path to a local file to upload. Must be inside the kojo upload directory ({tmp}/kojo/upload). Preferred for binary and large files — avoids base64 overhead.","type":"string"},"filename":{"description":"Name of the file (e.g. 'report.csv', 'image.png')","type":"string"},"initial_comment":{"description":"Message to post alongside the file","type":"string"},"thread_ts":{"description":"Thread timestamp to upload the file as a reply","type":"string"},"title":{"description":"Title of the file (defaults to filename)","type":"string"}},"required":["channel","filename"],"type":"object"},"name":"slack_upload_file"}
]`

func TestMCPToolsList(t *testing.T) {
	var tokenCalls atomic.Int32
	m := startMCP(t, mcpOptions{BotToken: func(context.Context) (string, error) {
		tokenCalls.Add(1)
		return "", errors.New("no token")
	}})

	var want []any
	if err := json.Unmarshal([]byte(builtinToolsJSON), &want); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"slack_add_reaction", "slack_get_channel_history", "slack_get_thread_replies",
		"slack_list_channels", "slack_list_emoji", "slack_list_users", "slack_post_message",
		"slack_remove_reaction", "slack_reply_to_thread", "slack_upload_file",
	}

	res := m.result("tools/list", nil)
	tools, _ := res["tools"].([]any)
	if len(tools) != len(wantNames) || len(want) != len(wantNames) {
		t.Fatalf("tools/list returned %d tools, reference has %d, want %d", len(tools), len(want), len(wantNames))
	}
	for i, name := range wantNames {
		tool, _ := tools[i].(map[string]any)
		if tool["name"] != name {
			t.Errorf("tool %d = %v, want %s", i, tool["name"], name)
			continue
		}
		// Name, description, input schema (including the always-present
		// "required" array) and annotations, compared as decoded JSON.
		if !reflect.DeepEqual(tool, want[i]) {
			got, _ := json.Marshal(tool)
			ref, _ := json.Marshal(want[i])
			t.Errorf("%s differs from the built-in definition:\n got %s\nwant %s", name, got, ref)
		}
	}

	if n := tokenCalls.Load(); n != 0 {
		t.Errorf("bot token was fetched %d times before any tool call", n)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("malformed request", func(t *testing.T) {
		m.send(`{"jsonrpc":"2.0","id":1,"method":`)
		resp := m.recv()
		wantRPCError(t, resp, -32700, "parse error")
		if resp["id"] != nil {
			t.Errorf("id = %v, want null", resp["id"])
		}
	})

	t.Run("unknown method", func(t *testing.T) {
		wantRPCError(t, m.request("resources/list", nil), -32601, "method not found: resources/list")
	})

	t.Run("notifications and blank lines get no answer", func(t *testing.T) {
		m.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		m.send("")
		m.send(`{"jsonrpc":"2.0","method":"notifications/unknown"}`)
		// request checks the id, so an answer to either notification
		// would be caught here.
		m.result("ping", nil)
	})

	t.Run("unknown tool", func(t *testing.T) {
		resp := m.request("tools/call", map[string]any{"name": "slack_delete_everything", "arguments": map[string]any{}})
		wantRPCError(t, resp, -32602, "tool 'slack_delete_everything' not found: tool not found")
	})

	t.Run("tools/call with malformed params", func(t *testing.T) {
		resp := m.request("tools/call", map[string]any{"name": "slack_list_emoji", "arguments": "not an object"})
		wantRPCError(t, resp, -32602, "invalid params")
	})
}

func TestMCPMissingToken(t *testing.T) {
	const want = "Slack bot token is not configured for this agent"

	t.Run("no token source", func(t *testing.T) {
		m := startMCP(t, mcpOptions{})
		m.wantError("slack_list_emoji", nil, want)
	})

	t.Run("every tool reports it before validating arguments", func(t *testing.T) {
		m := startMCP(t, mcpOptions{BotToken: func(context.Context) (string, error) { return "", nil }})
		for _, tool := range mcpTools {
			m.wantError(tool.Name, map[string]any{}, want)
		}
	})

	t.Run("lookup is retried after a failure and cached after a success", func(t *testing.T) {
		f := newFakeSlack(t)
		f.reply("emoji.list", `{"ok":true,"emoji":{}}`)
		opts := f.options()
		var calls atomic.Int32
		opts.BotToken = func(context.Context) (string, error) {
			switch calls.Add(1) {
			case 1:
				return "", nil
			case 2:
				return "", errors.New("kojo is unreachable")
			}
			return testBotToken, nil
		}
		m := startMCP(t, opts)

		m.wantError("slack_list_emoji", nil, want)
		m.wantError("slack_list_emoji", nil, want)
		m.wantText("slack_list_emoji", nil, "null")
		m.wantText("slack_list_emoji", nil, "null")
		if n := calls.Load(); n != 3 {
			t.Errorf("bot token fetched %d times, want 3 (the success must be cached)", n)
		}
		if got := f.lastForm(t, "emoji.list").Get("token"); got != testBotToken {
			t.Errorf("token sent to Slack = %q, want the bot token", got)
		}

		// A token Slack refuses is forgotten, so a replaced token is
		// picked up by the next call.
		f.reply("emoji.list", `{"ok":false,"error":"invalid_auth"}`)
		m.wantError("slack_list_emoji", nil, "Slack API error: invalid_auth")
		f.reply("emoji.list", `{"ok":true,"emoji":{}}`)
		m.wantText("slack_list_emoji", nil, "null")
		if n := calls.Load(); n != 4 {
			t.Errorf("bot token fetched %d times, want 4 (refetch after invalid_auth)", n)
		}
	})
}

func TestMCPListChannels(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("conversations.list", channelsReply)
		m.wantText("slack_list_channels", nil, `[
  {
    "id": "C0123456789",
    "name": "general",
    "topic": "chat",
    "purpose": "everything",
    "numMembers": 3,
    "isMember": true
  }
]`)
		form := f.lastForm(t, "conversations.list")
		if form.Get("types") != "public_channel,private_channel" || form.Get("limit") != "200" || form.Get("exclude_archived") != "true" {
			t.Errorf("conversations.list params = %v", form)
		}

		m.wantText("slack_list_channels", map[string]any{"member_only": false, "name_contains": "RAND"}, `[
  {
    "id": "C0987654321",
    "name": "random",
    "numMembers": 7,
    "isMember": false
  }
]`)
		// No match is rendered as JSON null, as the built-in tool did.
		m.wantText("slack_list_channels", map[string]any{"name_contains": "nothing"}, "null")
	})

	t.Run("failure", func(t *testing.T) {
		f.reply("conversations.list", `{"ok":false,"error":"missing_scope"}`)
		m.wantError("slack_list_channels", nil, "Slack API error: missing_scope")
	})
}

func TestMCPPostMessage(t *testing.T) {
	f := newFakeSlack(t)
	f.reply("conversations.list", channelsReply)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("chat.postMessage", `{"ok":true,"channel":"C0123456789","ts":"1700000000.000100"}`)
		m.wantText("slack_post_message", map[string]any{"channel": "#general", "text": "hello *world*"},
			`{"channel":"C0123456789","status":"posted","timestamp":"1700000000.000100"}`)
		form := f.lastForm(t, "chat.postMessage")
		if form.Get("channel") != testChannelID || form.Get("text") != "hello *world*" {
			t.Errorf("chat.postMessage params = %v", form)
		}

		// A conversation ID is used as it is, without a lookup.
		before := f.callCount("conversations.list")
		m.wantText("slack_post_message", map[string]any{"channel": testChannelID, "text": "again"},
			`{"channel":"C0123456789","status":"posted","timestamp":"1700000000.000100"}`)
		if after := f.callCount("conversations.list"); after != before {
			t.Errorf("conversations.list called %d times for a channel ID", after-before)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_post_message", map[string]any{"channel": testChannelID},
			"both 'channel' and 'text' are required")
		m.wantError("slack_post_message", map[string]any{"channel": "#nope", "text": "hi"},
			`failed to resolve channel "#nope": channel "nope" not found`)
		f.reply("chat.postMessage", `{"ok":false,"error":"not_in_channel"}`)
		m.wantError("slack_post_message", map[string]any{"channel": testChannelID, "text": "hi"},
			"failed to post message: not_in_channel")
	})
}

func TestMCPReplyToThread(t *testing.T) {
	f := newFakeSlack(t)
	f.reply("conversations.list", channelsReply)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("chat.postMessage", `{"ok":true,"channel":"C0123456789","ts":"1700000001.000200"}`)
		args := map[string]any{"channel": "general", "thread_ts": "1700000000.000100", "text": "a reply"}
		const want = `{"channel":"C0123456789","status":"replied","thread_ts":"1700000000.000100","timestamp":"1700000001.000200"}`
		m.wantText("slack_reply_to_thread", args, want)
		form := f.lastForm(t, "chat.postMessage")
		if form.Get("channel") != testChannelID || form.Get("thread_ts") != "1700000000.000100" || form.Get("text") != "a reply" {
			t.Errorf("chat.postMessage params = %v", form)
		}
		if form.Has("reply_broadcast") {
			t.Errorf("reply was broadcast without being asked to: %v", form)
		}

		args["broadcast"] = true
		m.wantText("slack_reply_to_thread", args, want)
		if got := f.lastForm(t, "chat.postMessage").Get("reply_broadcast"); got != "true" {
			t.Errorf("reply_broadcast = %q, want true", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_reply_to_thread", map[string]any{"channel": testChannelID, "text": "hi"},
			"'channel', 'thread_ts', and 'text' are all required")
		f.reply("chat.postMessage", `{"ok":false,"error":"thread_not_found"}`)
		m.wantError("slack_reply_to_thread",
			map[string]any{"channel": testChannelID, "thread_ts": "1700000000.000100", "text": "hi"},
			"failed to post reply: thread_not_found")
	})
}

func TestMCPAddReaction(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("reactions.add", `{"ok":true}`)
		m.wantText("slack_add_reaction",
			map[string]any{"channel": testChannelID, "timestamp": "1700000000.000100", "emoji": ":thumbsup:"},
			`{"emoji":"thumbsup","status":"added"}`)
		form := f.lastForm(t, "reactions.add")
		if form.Get("name") != "thumbsup" || form.Get("channel") != testChannelID || form.Get("timestamp") != "1700000000.000100" {
			t.Errorf("reactions.add params = %v", form)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_add_reaction", map[string]any{"channel": testChannelID, "emoji": "eyes"},
			"'channel', 'timestamp', and 'emoji' are all required")
		f.reply("reactions.add", `{"ok":false,"error":"already_reacted"}`)
		m.wantError("slack_add_reaction",
			map[string]any{"channel": testChannelID, "timestamp": "1700000000.000100", "emoji": "eyes"},
			"failed to add reaction: already_reacted")
	})
}

func TestMCPRemoveReaction(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("reactions.remove", `{"ok":true}`)
		m.wantText("slack_remove_reaction",
			map[string]any{"channel": testChannelID, "timestamp": "1700000000.000100", "emoji": ":eyes:"},
			`{"emoji":"eyes","status":"removed"}`)
		form := f.lastForm(t, "reactions.remove")
		if form.Get("name") != "eyes" || form.Get("channel") != testChannelID || form.Get("timestamp") != "1700000000.000100" {
			t.Errorf("reactions.remove params = %v", form)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_remove_reaction", map[string]any{"timestamp": "1700000000.000100", "emoji": "eyes"},
			"'channel', 'timestamp', and 'emoji' are all required")
		f.reply("reactions.remove", `{"ok":false,"error":"no_reaction"}`)
		m.wantError("slack_remove_reaction",
			map[string]any{"channel": testChannelID, "timestamp": "1700000000.000100", "emoji": "eyes"},
			"failed to remove reaction: no_reaction")
	})
}

func TestMCPListEmoji(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("emoji.list", `{"ok":true,"emoji":{"shipit":"alias:squirrel","partyparrot":"https://emoji.example.com/partyparrot.gif","PartyHat":"https://emoji.example.com/partyhat.png"}}`)
		m.wantText("slack_list_emoji", nil, `[
  {
    "name": "PartyHat",
    "value": "https://emoji.example.com/partyhat.png"
  },
  {
    "name": "partyparrot",
    "value": "https://emoji.example.com/partyparrot.gif"
  },
  {
    "name": "shipit",
    "value": "alias:squirrel"
  }
]`)
		m.wantText("slack_list_emoji", map[string]any{"name_contains": "SHIP"}, `[
  {
    "name": "shipit",
    "value": "alias:squirrel"
  }
]`)
	})

	t.Run("failure", func(t *testing.T) {
		f.reply("emoji.list", `{"ok":false,"error":"missing_scope"}`)
		m.wantError("slack_list_emoji", nil, "Slack API error: missing_scope")
	})
}

func TestMCPGetChannelHistory(t *testing.T) {
	f := newFakeSlack(t)
	f.reply("conversations.list", channelsReply)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("conversations.history", `{"ok":true,"messages":[`+
			`{"type":"message","user":"U1","text":"hello","ts":"1700000002.000300","thread_ts":"1700000002.000300","reply_count":2,"reactions":[{"name":"eyes","count":1,"users":["U2"]}]},`+
			`{"type":"message","subtype":"bot_message","bot_id":"B1","username":"ReleaseBot","text":"","ts":"1700000001.000200"}`+
			`]}`)
		m.wantText("slack_get_channel_history", map[string]any{"channel": "#general", "oldest": "1700000000", "latest": "1700009999"}, `[
  {
    "user": "U1",
    "text": "hello",
    "timestamp": "1700000002.000300",
    "thread_ts": "1700000002.000300",
    "reply_count": 2,
    "reactions": [
      {
        "name": "eyes",
        "count": 1,
        "users": [
          "U2"
        ]
      }
    ]
  },
  {
    "user": "",
    "username": "ReleaseBot",
    "bot_id": "B1",
    "text": "",
    "timestamp": "1700000001.000200",
    "subtype": "bot_message"
  }
]`)
		form := f.lastForm(t, "conversations.history")
		if form.Get("channel") != testChannelID || form.Get("limit") != "20" ||
			form.Get("oldest") != "1700000000" || form.Get("latest") != "1700009999" ||
			form.Get("include_all_metadata") != "1" {
			t.Errorf("conversations.history params = %v", form)
		}

		f.reply("conversations.history", `{"ok":true,"messages":[]}`)
		m.wantText("slack_get_channel_history", map[string]any{"channel": testChannelID, "limit": 5000}, "[]")
		if got := f.lastForm(t, "conversations.history").Get("limit"); got != "100" {
			t.Errorf("limit = %q, want it clamped to 100", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_get_channel_history", map[string]any{}, "'channel' is required")
		f.reply("conversations.history", `{"ok":false,"error":"not_in_channel"}`)
		m.wantError("slack_get_channel_history", map[string]any{"channel": testChannelID},
			"Slack API error: not_in_channel")
	})
}

func TestMCPGetThreadReplies(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("conversations.replies", `{"ok":true,"has_more":false,"messages":[`+
			`{"type":"message","user":"U1","text":"root","ts":"1700000000.000100","thread_ts":"1700000000.000100","reply_count":1},`+
			`{"type":"message","user":"U2","text":"reply","ts":"1700000001.000200","thread_ts":"1700000000.000100","edited":{"user":"U2","ts":"1700000003.000400"}}`+
			`]}`)
		m.wantText("slack_get_thread_replies", map[string]any{"channel": testChannelID, "thread_ts": "1700000000.000100"}, `[
  {
    "user": "U1",
    "text": "root",
    "timestamp": "1700000000.000100",
    "thread_ts": "1700000000.000100",
    "reply_count": 1
  },
  {
    "user": "U2",
    "text": "reply",
    "timestamp": "1700000001.000200",
    "thread_ts": "1700000000.000100",
    "edited": {
      "user": "U2",
      "ts": "1700000003.000400"
    }
  }
]`)
		form := f.lastForm(t, "conversations.replies")
		if form.Get("channel") != testChannelID || form.Get("ts") != "1700000000.000100" ||
			form.Get("limit") != "50" || form.Get("include_all_metadata") != "1" {
			t.Errorf("conversations.replies params = %v", form)
		}

		m.callTool("slack_get_thread_replies", map[string]any{"channel": testChannelID, "thread_ts": "1700000000.000100", "limit": 9999})
		if got := f.lastForm(t, "conversations.replies").Get("limit"); got != "200" {
			t.Errorf("limit = %q, want it clamped to 200", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_get_thread_replies", map[string]any{"channel": testChannelID},
			"'channel' and 'thread_ts' are required")
		f.reply("conversations.replies", `{"ok":false,"error":"thread_not_found"}`)
		m.wantError("slack_get_thread_replies", map[string]any{"channel": testChannelID, "thread_ts": "1700000000.000100"},
			"Slack API error: thread_not_found")
	})
}

func TestMCPListUsers(t *testing.T) {
	f := newFakeSlack(t)
	m := startMCP(t, f.options())

	t.Run("success", func(t *testing.T) {
		f.reply("users.list", `{"ok":true,"members":[`+
			`{"id":"U1","name":"alice","real_name":"Alice Smith","is_admin":true,"profile":{"display_name":"Alice"}},`+
			`{"id":"U2","name":"deploybot","real_name":"Deploy Bot","is_bot":true,"profile":{"display_name":""}},`+
			`{"id":"U3","name":"gone","real_name":"Gone","deleted":true},`+
			`{"id":"U4","name":"bob","real_name":"Bob Jones","profile":{"display_name":"Bobby"}}`+
			`],"response_metadata":{"next_cursor":""}}`)
		m.wantText("slack_list_users", nil, `[
  {
    "id": "U1",
    "name": "alice",
    "realName": "Alice Smith",
    "displayName": "Alice",
    "isAdmin": true
  },
  {
    "id": "U4",
    "name": "bob",
    "realName": "Bob Jones",
    "displayName": "Bobby"
  }
]`)
		m.wantText("slack_list_users", map[string]any{"include_bots": true, "name_contains": "BOT"}, `[
  {
    "id": "U2",
    "name": "deploybot",
    "realName": "Deploy Bot",
    "displayName": "",
    "isBot": true
  }
]`)
		m.wantText("slack_list_users", map[string]any{"limit": 1}, `[
  {
    "id": "U1",
    "name": "alice",
    "realName": "Alice Smith",
    "displayName": "Alice",
    "isAdmin": true
  }
]`)
		// No match is rendered as JSON null, as the built-in tool did.
		m.wantText("slack_list_users", map[string]any{"name_contains": "nobody"}, "null")
	})

	t.Run("failure", func(t *testing.T) {
		f.reply("users.list", `{"ok":false,"error":"missing_scope"}`)
		m.wantError("slack_list_users", nil, "Slack API error: missing_scope")
	})
}

func TestMCPUploadFile(t *testing.T) {
	tmpRoot := t.TempDir()
	uploadRoot := filepath.Join(tmpRoot, "upload")
	if err := os.MkdirAll(uploadRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(uploadRoot, "report.csv")
	if err := os.WriteFile(inside, []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmpRoot, "secret.txt")
	if err := os.WriteFile(outside, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeSlack(t)
	f.reply("conversations.list", channelsReply)
	f.reply("files.getUploadURLExternal", `{"ok":true,"upload_url":"`+f.srv.URL+`/upload","file_id":"`+testFileID+`"}`)
	opts := f.options()
	opts.UploadDir = uploadRoot
	m := startMCP(t, opts)

	lastUpload := func(t *testing.T) fakeUpload {
		t.Helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.uploads) == 0 {
			t.Fatal("nothing was uploaded")
		}
		return f.uploads[len(f.uploads)-1]
	}

	t.Run("success with content", func(t *testing.T) {
		f.reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"`+testFileID+`","title":"notes.txt"}]}`)
		m.wantText("slack_upload_file", map[string]any{
			"channel": "#general", "filename": "notes.txt", "content": "some notes",
			"initial_comment": "here you go", "thread_ts": "1700000000.000100",
		}, `{"channel":"C0123456789","file_id":"F0123456789","status":"uploaded","title":"notes.txt"}`)

		form := f.lastForm(t, "files.getUploadURLExternal")
		if form.Get("filename") != "notes.txt" || form.Get("length") != "10" {
			t.Errorf("files.getUploadURLExternal params = %v", form)
		}
		up := lastUpload(t)
		if up.filename != "notes.txt" || up.body != "some notes" || up.auth != "Bearer "+testBotToken {
			t.Errorf("upload = %+v", up)
		}
		form = f.lastForm(t, "files.completeUploadExternal")
		if form.Get("channel_id") != testChannelID || form.Get("initial_comment") != "here you go" ||
			form.Get("thread_ts") != "1700000000.000100" {
			t.Errorf("files.completeUploadExternal params = %v", form)
		}
		// The title defaults to the filename.
		if got, want := form.Get("files"), `[{"id":"F0123456789","title":"notes.txt"}]`; got != want {
			t.Errorf("files = %s, want %s", got, want)
		}
	})

	t.Run("success with file_path", func(t *testing.T) {
		f.reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"`+testFileID+`","title":"Q4"}]}`)
		m.wantText("slack_upload_file", map[string]any{
			"channel": testChannelID, "filename": "report.csv", "file_path": inside, "title": "Q4",
		}, `{"channel":"C0123456789","file_id":"F0123456789","status":"uploaded","title":"Q4"}`)

		if got := f.lastForm(t, "files.getUploadURLExternal").Get("length"); got != "8" {
			t.Errorf("length = %q, want the file size 8", got)
		}
		if up := lastUpload(t); up.filename != "report.csv" || up.body != "a,b\n1,2\n" {
			t.Errorf("upload = %+v", up)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m.wantError("slack_upload_file", map[string]any{"channel": testChannelID, "content": "x"},
			"'channel' and 'filename' are required")
		m.wantError("slack_upload_file", map[string]any{"channel": testChannelID, "filename": "a.txt"},
			"one of 'file_path' or 'content' is required")
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "a.txt", "content": "x", "file_path": inside},
			"provide only one of 'file_path' or 'content'")
		m.wantError("slack_upload_file",
			map[string]any{"channel": "#nope", "filename": "a.txt", "content": "x"},
			`failed to resolve channel "#nope": channel "nope" not found`)

		f.mu.Lock()
		before := len(f.uploads)
		f.mu.Unlock()
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "secret.txt", "file_path": outside},
			"'file_path' must be inside the kojo upload directory")
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "gone.txt", "file_path": filepath.Join(uploadRoot, "gone.txt")},
			"'file_path' does not exist or is not accessible")
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "dir", "file_path": uploadRoot},
			"'file_path' must be a regular file, not a directory")
		f.mu.Lock()
		after := len(f.uploads)
		f.mu.Unlock()
		if after != before {
			t.Errorf("%d uploads happened for refused paths", after-before)
		}

		f.reply("files.completeUploadExternal", `{"ok":false,"error":"channel_not_found"}`)
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "a.txt", "content": "x"},
			"failed to upload file: CompleteUploadExternal: channel_not_found")

		f.mu.Lock()
		f.uploadStatus = http.StatusInternalServerError
		f.mu.Unlock()
		m.wantError("slack_upload_file",
			map[string]any{"channel": testChannelID, "filename": "a.txt", "content": "x"},
			"failed to upload file: UploadToURL: upload returned HTTP 500")
	})
}

// Without an upload directory no file_path may be read: an empty root must
// not turn the containment check into "any absolute path".
func TestMCPUploadFileWithoutUploadDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, _, kind := openUploadPath("", p); f != nil || kind != uploadErrOutside {
		if f != nil {
			f.Close()
		}
		t.Fatalf("openUploadPath with no upload dir: kind = %q, want %q", kind, uploadErrOutside)
	}

	f := newFakeSlack(t)
	m := startMCP(t, f.options())
	m.wantError("slack_upload_file",
		map[string]any{"channel": testChannelID, "filename": "file.txt", "file_path": p},
		"'file_path' must be inside the kojo upload directory")
	if n := f.callCount("files.getUploadURLExternal"); n != 0 {
		t.Errorf("files.getUploadURLExternal called %d times for a refused path", n)
	}
}
