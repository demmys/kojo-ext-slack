// kojo-ext-slack bridges a Slack app to a kojo agent.
//
// The same binary runs in two modes:
//
//	kojo-ext-slack        the supervised service: holds the Socket Mode
//	                      connection and turns Slack messages into agent
//	                      turns
//	kojo-ext-slack mcp    a stdio MCP server the agent uses to act on
//	                      Slack on its own initiative
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo-ext-slack/internal/kojoclient"
	"github.com/loppo-llc/kojo-ext-slack/internal/slackbot"
)

// version is stamped by the Makefile.
var version = "dev"

const settingsHint = "Set it in kojo: Settings → Extensions → Slack → this agent"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	var err error
	switch mode {
	case "":
		err = runService(ctx, loadEnv(), log)
	case "mcp":
		err = runMCPMode(ctx, loadEnv(), log)
	case "version", "--version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		log.Error("kojo-ext-slack stopped", "err", err)
		os.Exit(1)
	}
}

func runService(ctx context.Context, env Env, log *slog.Logger) error {
	if env.APIBase == "" || env.Token == "" || env.AgentID == "" || env.DataDir == "" {
		return errors.New("KOJO_API_BASE, KOJO_EXT_TOKEN, KOJO_EXT_AGENT_ID and KOJO_EXT_DATA_DIR are required; this binary is started by kojo")
	}
	log = log.With("agent", env.AgentID)
	if env.ConfigErr != nil {
		log.Warn("settings could not be read; using the defaults", "err", env.ConfigErr)
	}
	appToken := strings.TrimSpace(env.Secrets[secretAppToken])
	botToken := strings.TrimSpace(env.Secrets[secretBotToken])
	// Neither case below is worth a restart loop: kojo restarts the service
	// by itself once the operator saves new settings.
	if appToken == "" || botToken == "" {
		log.Warn("the app-level token and the bot token are both required; waiting. " + settingsHint)
		<-ctx.Done()
		return nil
	}
	if err := validateTokens(appToken, botToken); err != nil {
		log.Error("the Slack tokens are not usable; not connecting. "+settingsHint, "err", err)
		<-ctx.Done()
		return nil
	}
	if !waitForSlack(ctx, appToken, botToken, log) {
		<-ctx.Done()
		return nil
	}

	client := kojoclient.New(env.APIBase, env.Token, env.AgentID, log)
	bot := slackbot.NewBot(ctx, env.AgentID, env.DataDir, env.Config, appToken, botToken, client, log)
	bot.Run()
	if ctx.Err() == nil {
		// Returning an error has kojo restart the service.
		return errors.New("the Slack connection ended")
	}
	return nil
}

// waitForSlack checks the bot token against Slack before the bot starts.
// It returns true once Slack accepts the token and false when the token
// was refused or ctx ended. A Slack that cannot be reached is retried
// here: exiting instead would only move the retry loop into kojo's
// supervisor, with its restart limits.
func waitForSlack(ctx context.Context, appToken, botToken string, log *slog.Logger) bool {
	for delay := time.Second; ; delay = min(delay*2, time.Minute) {
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		team, user, err := slackbot.TestConnection(checkCtx, appToken, botToken)
		cancel()
		if err == nil {
			log.Info("Slack accepted the bot token", "team", team, "botUser", user)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if tokenRefused(err) {
			log.Error("Slack refused the bot token; not connecting. Reinstall the app or copy the Bot User OAuth Token again. "+settingsHint, "err", err)
			return false
		}
		log.Warn("Slack could not be reached; retrying", "in", delay, "err", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
}

// refusals are the auth.test errors that only a new token can fix.
var refusals = map[string]bool{
	"invalid_auth":           true,
	"not_authed":             true,
	"account_inactive":       true,
	"token_revoked":          true,
	"token_expired":          true,
	"not_allowed_token_type": true,
}

func tokenRefused(err error) bool {
	var resp slack.SlackErrorResponse
	return errors.As(err, &resp) && refusals[resp.Err]
}

func runMCPMode(ctx context.Context, env Env, log *slog.Logger) error {
	if env.APIBase == "" || env.Token == "" || env.AgentID == "" {
		return errors.New("KOJO_API_BASE, KOJO_EXT_TOKEN_FILE and KOJO_EXT_AGENT_ID are required; this binary is started by kojo")
	}
	client := kojoclient.New(env.APIBase, env.Token, env.AgentID, log)
	return runMCP(ctx, os.Stdin, os.Stdout, log, version, mcpOptions{
		BotToken:  func(ctx context.Context) (string, error) { return fetchBotToken(ctx, client) },
		UploadDir: env.UploadDir,
	})
}

// secretsReader is the part of the kojo client the MCP server needs.
type secretsReader interface {
	Secrets(ctx context.Context) (map[string]string, error)
}

// fetchBotToken reads the agent's bot token from kojo. The MCP server is
// started by the agent's CLI, not by kojo's supervisor, so it is not handed
// KOJO_EXT_SECRETS and asks for the token when a tool first needs it.
func fetchBotToken(ctx context.Context, kojo secretsReader) (string, error) {
	secrets, err := kojo.Secrets(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the Slack bot token from kojo: %w", err)
	}
	token := strings.TrimSpace(secrets[secretBotToken])
	if token == "" {
		return "", errors.New("no Slack bot token is configured for this agent. " + settingsHint)
	}
	if !strings.HasPrefix(token, "xoxb-") {
		return "", errors.New("botToken must start with xoxb-. " + settingsHint)
	}
	return token, nil
}
