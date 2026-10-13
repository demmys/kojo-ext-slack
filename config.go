package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

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
