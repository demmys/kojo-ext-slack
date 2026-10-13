package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/loppo-llc/kojo-ext-slack/internal/uploadpath"
)

// Names of the secret settings, as in settings.schema.json.
const (
	secretAppToken = "appToken"
	secretBotToken = "botToken"
)

// Env is the KOJO_EXT_* contract, read once at startup.
type Env struct {
	APIBase   string
	Token     string
	AgentID   string
	DataDir   string
	UploadDir string
	Config    agent.SlackBotConfig
	// ConfigErr is set when KOJO_EXT_CONFIG could not be read and Config
	// holds the defaults instead.
	ConfigErr error
	Secrets   map[string]string
}

func loadEnv() Env {
	e := Env{
		APIBase:   strings.TrimRight(os.Getenv("KOJO_API_BASE"), "/"),
		Token:     os.Getenv("KOJO_EXT_TOKEN"),
		AgentID:   os.Getenv("KOJO_EXT_AGENT_ID"),
		DataDir:   os.Getenv("KOJO_EXT_DATA_DIR"),
		UploadDir: os.Getenv(uploadpath.EnvDir),
		Secrets:   map[string]string{},
	}
	e.Config, e.ConfigErr = parseConfig(os.Getenv("KOJO_EXT_CONFIG"))
	if raw := os.Getenv("KOJO_EXT_SECRETS"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &e.Secrets)
	}
	// An MCP server gets the token as a file only: its environment is
	// assembled on the backend CLI's command line, where `ps` can see it.
	if file := os.Getenv("KOJO_EXT_TOKEN_FILE"); e.Token == "" && file != "" {
		if data, err := os.ReadFile(file); err == nil {
			e.Token = strings.TrimSpace(string(data))
		}
	}
	return e
}

// parseConfig reads the per-agent settings object. Every switch defaults to
// on, which is what the built-in integration did for an agent whose
// settings were never touched. Settings that cannot be read give the
// defaults and an error: there is nothing in them that restricts who the
// bot answers, so running on defaults is safe.
func parseConfig(raw string) (agent.SlackBotConfig, error) {
	var in struct {
		ThreadReplies  *bool `json:"threadReplies"`
		RespondDM      *bool `json:"respondDM"`
		RespondMention *bool `json:"respondMention"`
		RespondThread  *bool `json:"respondThread"`
	}
	var err error
	if strings.TrimSpace(raw) != "" {
		if err = json.Unmarshal([]byte(raw), &in); err != nil {
			in.ThreadReplies, in.RespondDM, in.RespondMention, in.RespondThread = nil, nil, nil, nil
			err = fmt.Errorf("KOJO_EXT_CONFIG is not a settings object: %w", err)
		}
	}
	return agent.SlackBotConfig{
		Enabled:        true,
		ThreadReplies:  in.ThreadReplies == nil || *in.ThreadReplies,
		RespondDM:      in.RespondDM,
		RespondMention: in.RespondMention,
		RespondThread:  in.RespondThread,
	}, err
}

// validateTokens catches the usual mistake of pasting a token into the
// wrong field (or pasting a signing secret) before anything connects.
// envSlackAPIURL names a variable that points the Slack Web API client
// at another server. It exists for tests that run the package against a
// fake Slack; nothing in kojo sets it. The name is outside the KOJO_
// prefix because kojo reserves that prefix: a package manifest's "env"
// block, which is how a test hands the variable to both the service and
// the MCP server, cannot set a KOJO_ name.
const envSlackAPIURL = "EXT_SLACK_API_URL"

// slackAPIOptions returns the client options for the override above, or
// none. Socket Mode follows along by itself: the WebSocket address is
// whatever apps.connections.open answers, and that call goes through the
// same client.
func slackAPIOptions(log *slog.Logger) []slack.Option {
	u := strings.TrimSpace(os.Getenv(envSlackAPIURL))
	if u == "" {
		return nil
	}
	if !strings.HasSuffix(u, "/") {
		// slack-go appends the method name to the URL as it stands.
		u += "/"
	}
	// Not the URL itself: it may carry credentials.
	host := "unparseable"
	if p, err := url.Parse(u); err == nil {
		host = p.Host
	}
	log.Warn("using a Slack API address override; this is for testing only", "env", envSlackAPIURL, "host", host)
	return []slack.Option{slack.OptionAPIURL(u)}
}

func validateTokens(appToken, botToken string) error {
	var errs []error
	if !strings.HasPrefix(appToken, "xapp-") {
		errs = append(errs, errors.New("appToken must start with xapp-"))
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		errs = append(errs, errors.New("botToken must start with xoxb-"))
	}
	return errors.Join(errs...)
}
