package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func boolValue(p *bool) string {
	if p == nil {
		return "unset"
	}
	return fmt.Sprint(*p)
}

func TestParseConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		raw                 string
		threadReplies       bool
		dm, mention, thread string
		wantErr             bool
	}{
		"empty":        {"", true, "unset", "unset", "unset", false},
		"blank":        {"  \n", true, "unset", "unset", "unset", false},
		"empty object": {"{}", true, "unset", "unset", "unset", false},
		"all off": {`{"threadReplies":false,"respondDM":false,"respondMention":false,"respondThread":false}`,
			false, "false", "false", "false", false},
		"mixed":      {`{"threadReplies":true,"respondDM":false,"respondMention":true}`, true, "false", "true", "unset", false},
		"extra keys": {`{"respondThread":false,"somethingElse":1}`, true, "unset", "unset", "false", false},
		"not json":   {`{"respondDM":`, true, "unset", "unset", "unset", true},
		"wrong type": {`{"respondDM":"no","threadReplies":false}`, true, "unset", "unset", "unset", true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseConfig(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !cfg.Enabled {
				t.Error("Enabled = false; a running service is an enabled bot")
			}
			if cfg.ThreadReplies != tc.threadReplies {
				t.Errorf("ThreadReplies = %v, want %v", cfg.ThreadReplies, tc.threadReplies)
			}
			got := []string{boolValue(cfg.RespondDM), boolValue(cfg.RespondMention), boolValue(cfg.RespondThread)}
			want := []string{tc.dm, tc.mention, tc.thread}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("respondDM, respondMention, respondThread = %v, want %v", got, want)
			}
		})
	}
}

func TestValidateTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		app, bot string
		want     []string
	}{
		"both fine":  {"xapp-1-A", "xoxb-1-B", nil},
		"swapped":    {"xoxb-1-B", "xapp-1-A", []string{"appToken must start with xapp-", "botToken must start with xoxb-"}},
		"user token": {"xapp-1-A", "xoxp-1-B", []string{"botToken must start with xoxb-"}},
		"bad app":    {"secret", "xoxb-1-B", []string{"appToken must start with xapp-"}},
		"upper case": {"XAPP-1-A", "xoxb-1-B", []string{"appToken must start with xapp-"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateTokens(tc.app, tc.bot)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || err.Error() != strings.Join(tc.want, "\n") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KOJO_API_BASE", "KOJO_EXT_TOKEN", "KOJO_EXT_TOKEN_FILE", "KOJO_EXT_AGENT_ID",
		"KOJO_EXT_DATA_DIR", "KOJO_UPLOAD_DIR", "KOJO_EXT_CONFIG", "KOJO_EXT_SECRETS",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("KOJO_API_BASE", "http://127.0.0.1:8080/")
	t.Setenv("KOJO_EXT_TOKEN", "tok")
	t.Setenv("KOJO_EXT_AGENT_ID", "ag_1")
	t.Setenv("KOJO_EXT_DATA_DIR", "/data")
	t.Setenv("KOJO_UPLOAD_DIR", "/upload")
	t.Setenv("KOJO_EXT_CONFIG", `{"respondDM":false}`)
	t.Setenv("KOJO_EXT_SECRETS", `{"appToken":"xapp-1","botToken":"xoxb-1"}`)
	env := loadEnv()
	if env.APIBase != "http://127.0.0.1:8080" || env.Token != "tok" || env.AgentID != "ag_1" || env.DataDir != "/data" || env.UploadDir != "/upload" {
		t.Fatalf("env = %+v", env)
	}
	if env.ConfigErr != nil || boolValue(env.Config.RespondDM) != "false" || !env.Config.ThreadReplies {
		t.Fatalf("config = %+v, err %v", env.Config, env.ConfigErr)
	}
	if env.Secrets[secretAppToken] != "xapp-1" || env.Secrets[secretBotToken] != "xoxb-1" {
		t.Fatalf("secrets = %v", env.Secrets)
	}
}

func TestLoadEnvReadsTheTokenFile(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOJO_EXT_TOKEN_FILE", file)
	if env := loadEnv(); env.Token != "from-file" {
		t.Fatalf("Token = %q, want the file's content", env.Token)
	}
	// The variable wins when both are set: the service is given both.
	t.Setenv("KOJO_EXT_TOKEN", "from-env")
	if env := loadEnv(); env.Token != "from-env" {
		t.Fatalf("Token = %q, want the variable's value", env.Token)
	}
}

func TestTokenRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"invalid auth":    {fmt.Errorf("auth.test failed: %w", slack.SlackErrorResponse{Err: "invalid_auth"}), true},
		"revoked":         {slack.SlackErrorResponse{Err: "token_revoked"}, true},
		"slack trouble":   {slack.SlackErrorResponse{Err: "internal_error"}, false},
		"rate limited":    {&slack.RateLimitedError{RetryAfter: time.Second}, false},
		"network failure": {errors.New("dial tcp: connection refused"), false},
	} {
		if got := tokenRefused(tc.err); got != tc.want {
			t.Errorf("%s: tokenRefused = %v, want %v", name, got, tc.want)
		}
	}
}

type fakeSecrets struct {
	secrets map[string]string
	err     error
}

func (f fakeSecrets) Secrets(context.Context) (map[string]string, error) { return f.secrets, f.err }

func TestFetchBotToken(t *testing.T) {
	ctx := context.Background()
	got, err := fetchBotToken(ctx, fakeSecrets{secrets: map[string]string{"botToken": " xoxb-1 \n", "appToken": "xapp-1"}})
	if err != nil || got != "xoxb-1" {
		t.Fatalf("token = %q, %v", got, err)
	}
	for name, tc := range map[string]struct {
		src  fakeSecrets
		want string
	}{
		"kojo error": {fakeSecrets{err: errors.New("forbidden")}, "forbidden"},
		"not set":    {fakeSecrets{secrets: map[string]string{"appToken": "xapp-1"}}, "no Slack bot token is configured"},
		"wrong kind": {fakeSecrets{secrets: map[string]string{"botToken": "xapp-1"}}, "botToken must start with xoxb-"},
	} {
		if _, err := fetchBotToken(ctx, tc.src); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// syncBuffer is a log sink the test can read while the service writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Unusable settings must leave the service waiting, not exiting: an exit
// would have kojo restart it in a loop until the operator fixes them.
func TestRunServiceWaitsOnUnusableTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		secrets map[string]string
		logs    []string
	}{
		"no tokens":     {map[string]string{}, []string{"both required"}},
		"no app token":  {map[string]string{"botToken": "xoxb-1"}, []string{"both required"}},
		"no bot token":  {map[string]string{"appToken": "xapp-1"}, []string{"both required"}},
		"swapped":       {map[string]string{"appToken": "xoxb-1", "botToken": "xapp-1"}, []string{"appToken must start with xapp-", "botToken must start with xoxb-"}},
		"bad bot token": {map[string]string{"appToken": "xapp-1", "botToken": "xoxp-1"}, []string{"botToken must start with xoxb-"}},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			env := Env{
				// Nothing listens here: connecting would fail the test.
				APIBase: "http://127.0.0.1:1", Token: "tok", AgentID: "ag_1", DataDir: t.TempDir(),
				Secrets: tc.secrets,
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runService(ctx, env, slog.New(slog.NewTextHandler(logs, nil))) }()
			select {
			case err := <-done:
				t.Fatalf("runService returned %v before it was stopped", err)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runService = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runService did not return after its context ended")
			}
			for _, want := range tc.logs {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log does not mention %q:\n%s", want, logs.String())
				}
			}
		})
	}
}

func TestRunServiceRequiresTheKojoEnvironment(t *testing.T) {
	err := runService(context.Background(), Env{APIBase: "http://127.0.0.1:1", Token: "tok", AgentID: "ag_1"}, slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))
	if err == nil || !strings.Contains(err.Error(), "KOJO_EXT_DATA_DIR") {
		t.Fatalf("err = %v", err)
	}
}
