package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

// The MCP server speaks JSON-RPC 2.0 over stdio, one message per line.
// It serves the same ten Slack tools as kojo's built-in MCP endpoint, for
// the single agent this process belongs to, using only Slack's Web API.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

const mcpServerName = "slack"

const mcpProtocolVersion = "2024-11-05"

// mcpSupportedVersions are the protocol revisions this server speaks.
// Only tools/list and tools/call are implemented, and those have the
// same shape in all of them.
var mcpSupportedVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// errNoBotToken is the tool error for an agent without a usable bot token.
var errNoBotToken = errors.New("Slack bot token is not configured for this agent")

// slackAuthErrors are the Slack error codes that mean the token itself was
// refused. The built-in tools looked the token up on every call, so a
// replaced token took effect at once; forgetting the cached one on these
// errors keeps that behaviour.
var slackAuthErrors = map[string]bool{
	"invalid_auth":           true,
	"not_authed":             true,
	"account_inactive":       true,
	"token_revoked":          true,
	"token_expired":          true,
	"not_allowed_token_type": true,
}

// mcpOptions carries what the MCP server needs from its environment.
type mcpOptions struct {
	// BotToken returns the agent's Slack bot token (xoxb-). It is called
	// lazily and may be called again after a failure; a successful result
	// is cached.
	BotToken func(ctx context.Context) (string, error)
	// UploadDir is kojo's upload directory (KOJO_UPLOAD_DIR). It is the only
	// place slack_upload_file may read a file_path from; when empty, every
	// file_path is refused.
	UploadDir string
	// SlackOptions are passed to every Slack client; tests use them to
	// point the client at a fake Web API.
	SlackOptions []slack.Option
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema map[string]any  `json:"inputSchema"`
	Annotations map[string]bool `json:"annotations"`
	// call returns the text of a successful result. An error becomes a
	// tool result with isError set and the error's text as its content.
	call func(ctx context.Context, s *mcpServer, args map[string]any) (string, error)
}

type mcpServer struct {
	opts    mcpOptions
	version string
	log     *slog.Logger
	// token is the cached bot token. The serve loop handles one request at
	// a time, so it needs no lock.
	token string
}

func runMCP(ctx context.Context, in io.Reader, out io.Writer, logger *slog.Logger, version string, opts mcpOptions) error {
	if logger == nil {
		logger = slog.Default()
	}
	srv := &mcpServer{opts: opts, version: version, log: logger}
	return srv.serve(ctx, in, out)
}

func (s *mcpServer) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcParseError, Message: "parse error"}})
			continue
		}
		resp, reply := s.handle(ctx, req)
		if !reply {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	// A request without an id is a notification and gets no answer.
	notification := len(req.ID) == 0
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		// Agree to the client's revision when we speak it; otherwise
		// offer ours and let the client decide.
		pv := p.ProtocolVersion
		if !mcpSupportedVersions[pv] {
			pv = mcpProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": s.version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: rpcInvalidParams, Message: "invalid params"}
			break
		}
		result, rpcErr := s.callTool(ctx, p.Name, p.Arguments)
		if rpcErr != nil {
			resp.Error = rpcErr
			break
		}
		resp.Result = result
	default:
		if notification {
			return resp, false
		}
		resp.Error = &rpcError{Code: rpcMethodNotFound, Message: "method not found: " + req.Method}
	}
	return resp, !notification
}

// toolResult builds a tools/call result. isError is left out of a
// successful result, as the built-in server did.
func toolResult(text string, isError bool) map[string]any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
	if isError {
		res["isError"] = true
	}
	return res
}

func (s *mcpServer) callTool(ctx context.Context, name string, args map[string]any) (map[string]any, *rpcError) {
	for _, t := range mcpTools {
		if t.Name != name {
			continue
		}
		text, err := t.call(ctx, s, args)
		if err != nil {
			s.log.Warn("mcp tool error", "tool", name, "err", err)
			s.forgetRefusedToken(err)
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	// The built-in server answered an unknown tool name with a protocol
	// error rather than a tool result; the code and text are the same here.
	return nil, &rpcError{Code: rpcInvalidParams, Message: fmt.Sprintf("tool '%s' not found: tool not found", name)}
}

// forgetRefusedToken drops the cached bot token when err is Slack refusing
// the token itself.
func (s *mcpServer) forgetRefusedToken(err error) {
	var slackErr slack.SlackErrorResponse
	if errors.As(err, &slackErr) && slackAuthErrors[slackErr.Err] {
		s.token = ""
	}
}

// slackClient returns a Slack API client for the agent together with the
// bot token it uses. The token is fetched on first use, not at startup, so
// that tools/list works for an agent that has no token yet. A failure is
// not remembered: the token may be saved while this server is running.
func (s *mcpServer) slackClient(ctx context.Context) (*slack.Client, string, error) {
	if s.token == "" {
		if s.opts.BotToken == nil {
			return nil, "", errNoBotToken
		}
		token, err := s.opts.BotToken(ctx)
		if err != nil {
			s.log.Warn("mcp bot token lookup failed", "err", err)
			return nil, "", errNoBotToken
		}
		if token == "" {
			return nil, "", errNoBotToken
		}
		s.token = token
	}
	return slack.New(s.token, s.opts.SlackOptions...), s.token, nil
}

// object builds an input schema. "required" is always present, as an empty
// array when nothing is required: that is how the built-in server
// advertised its tools.
func object(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// mcpToolAnnotations are the hints the built-in server attached to every
// tool (its MCP library's defaults); they are advertised unchanged.
var mcpToolAnnotations = map[string]bool{
	"readOnlyHint":    false,
	"destructiveHint": true,
	"idempotentHint":  false,
	"openWorldHint":   true,
}

// mcpTools is sorted by name, the order the built-in server listed them in.
var mcpTools = []*mcpTool{
	{
		Name:        "slack_add_reaction",
		Description: "Add an emoji reaction to a message. The emoji name should NOT include colons (e.g. use 'thumbsup' not ':thumbsup:').",
		InputSchema: object([]string{"channel", "timestamp", "emoji"}, map[string]any{
			"channel":   prop("string", "Channel ID where the message is"),
			"timestamp": prop("string", "Timestamp of the message to react to"),
			"emoji":     prop("string", "Emoji name without colons (e.g. 'thumbsup', 'heart', 'eyes')"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolAddReaction,
	},
	{
		Name:        "slack_get_channel_history",
		Description: "Get recent messages from a Slack channel. Returns messages in reverse chronological order (newest first). Each message includes text, author (user/bot/app), timestamp, thread_ts, subtype, edited info, files (id/name/mimetype/size/permalink/url_private), reactions, attachments (title/text/fields/image_url/footer), Block Kit blocks (re-encoded JSON; unknown block types may be lossy), and metadata.",
		InputSchema: object([]string{"channel"}, map[string]any{
			"channel": prop("string", "Channel ID or #channel-name"),
			"limit":   prop("number", "Number of messages to retrieve (default 20, max 100)"),
			"oldest":  prop("string", "Only messages after this Unix timestamp"),
			"latest":  prop("string", "Only messages before this Unix timestamp"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolGetChannelHistory,
	},
	{
		Name:        "slack_get_thread_replies",
		Description: "Get all replies in a message thread. Each message includes text, author (user/bot/app), timestamp, edited info, files (id/name/mimetype/size/permalink/url_private), reactions, attachments (title/text/fields/image_url/footer), Block Kit blocks (re-encoded JSON; unknown block types may be lossy), and metadata.",
		InputSchema: object([]string{"channel", "thread_ts"}, map[string]any{
			"channel":   prop("string", "Channel ID where the thread exists"),
			"thread_ts": prop("string", "Timestamp of the parent message (thread root)"),
			"limit":     prop("number", "Maximum number of replies to return (default 50, max 200)"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolGetThreadReplies,
	},
	{
		Name:        "slack_list_channels",
		Description: "List Slack channels the bot can access. Returns channel ID, name, topic, and member count. By default returns only channels the bot has joined. Use member_only=false to include all visible channels (may be slow for large workspaces).",
		InputSchema: object(nil, map[string]any{
			"limit":         prop("number", "Maximum number of channels to return (default 200, max 1000)"),
			"name_contains": prop("string", "Filter channels whose name contains this substring (case-insensitive)"),
			"member_only":   prop("boolean", "If true (default), only return channels the bot has joined"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolListChannels,
	},
	{
		Name:        "slack_list_emoji",
		Description: "List custom emoji available in the Slack workspace. Returns emoji names and their image URLs or aliases. Standard Unicode emoji (like :thumbsup:) are always available and not included in this list.",
		InputSchema: object(nil, map[string]any{
			"name_contains": prop("string", "Filter emoji whose name contains this substring (case-insensitive)"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolListEmoji,
	},
	{
		Name:        "slack_list_users",
		Description: "List users in the Slack workspace. Returns user ID, name, display name, real name, and bot/admin flags. Useful for resolving @mentions or finding user IDs.",
		InputSchema: object(nil, map[string]any{
			"name_contains": prop("string", "Filter users whose name or display name contains this substring (case-insensitive)"),
			"limit":         prop("number", "Maximum number of users to return (default 200, max 500)"),
			"include_bots":  prop("boolean", "If true, include bot users in the result (default false)"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolListUsers,
	},
	{
		Name:        "slack_post_message",
		Description: "Post a message to a Slack channel. The channel parameter can be a channel ID (C0123ABC) or a #channel-name.",
		InputSchema: object([]string{"channel", "text"}, map[string]any{
			"channel": prop("string", "Channel ID or #channel-name to post to"),
			"text":    prop("string", "Message text (supports Slack mrkdwn formatting)"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolPostMessage,
	},
	{
		Name:        "slack_remove_reaction",
		Description: "Remove an emoji reaction from a message.",
		InputSchema: object([]string{"channel", "timestamp", "emoji"}, map[string]any{
			"channel":   prop("string", "Channel ID where the message is"),
			"timestamp": prop("string", "Timestamp of the message"),
			"emoji":     prop("string", "Emoji name without colons (e.g. 'thumbsup')"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolRemoveReaction,
	},
	{
		Name:        "slack_reply_to_thread",
		Description: "Post a reply to a specific thread in a Slack channel. Use this when you want to reply in a thread rather than posting a new top-level message.",
		InputSchema: object([]string{"channel", "thread_ts", "text"}, map[string]any{
			"channel":   prop("string", "Channel ID or #channel-name where the thread exists"),
			"thread_ts": prop("string", "Timestamp of the parent message (thread root)"),
			"text":      prop("string", "Reply text (supports Slack mrkdwn formatting)"),
			"broadcast": prop("boolean", "If true, also post the reply to the channel (default false)"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolReplyToThread,
	},
	{
		Name:        "slack_upload_file",
		Description: "Upload a file to a Slack channel. Provide file_path (preferred for binary/large files) or content (for text snippets). Only one source allowed.",
		InputSchema: object([]string{"channel", "filename"}, map[string]any{
			"channel":         prop("string", "Channel ID or #channel-name to upload to"),
			"filename":        prop("string", "Name of the file (e.g. 'report.csv', 'image.png')"),
			"file_path":       prop("string", "Path to a local file to upload. Must be inside the kojo upload directory ({tmp}/kojo/upload). Preferred for binary and large files — avoids base64 overhead."),
			"content":         prop("string", "Plain text content for the file (for text/code snippets). Use this OR file_path."),
			"title":           prop("string", "Title of the file (defaults to filename)"),
			"initial_comment": prop("string", "Message to post alongside the file"),
			"thread_ts":       prop("string", "Thread timestamp to upload the file as a reply"),
		}),
		Annotations: mcpToolAnnotations,
		call:        toolUploadFile,
	},
}

// ---------------------------------------------------------------------------
// slack_list_channels
// ---------------------------------------------------------------------------

func toolListChannels(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	opts := parseListChannelsArgs(args)

	s.log.Info("mcp tool invoked", "tool", "slack_list_channels",
		"limit", opts.Limit, "nameFilter", opts.NameFilter, "memberOnly", opts.MemberOnly)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	channels, err := listSlackChannels(ctx, api, opts, func(err error, collected int) {
		s.log.Warn("mcp tool partial", "tool", "slack_list_channels", "err", err, "collected", collected)
		// A partial result is a success, so callTool never sees this error.
		s.forgetRefusedToken(err)
	})
	if err != nil {
		return "", fmt.Errorf("Slack API error: %w", err)
	}

	data, _ := json.MarshalIndent(channels, "", "  ")
	s.log.Info("mcp tool result", "tool", "slack_list_channels", "channelCount", len(channels))
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_post_message
// ---------------------------------------------------------------------------

func toolPostMessage(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	text, _ := args["text"].(string)

	s.log.Info("mcp tool invoked", "tool", "slack_post_message",
		"channel", channel, "textLen", len(text))

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || text == "" {
		return "", errors.New("both 'channel' and 'text' are required")
	}

	resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
	if err != nil {
		return "", fmt.Errorf("failed to resolve channel %q: %w", channel, err)
	}

	_, ts, err := api.PostMessageContext(ctx, resolvedChannel, slack.MsgOptionText(text, false))
	if err != nil {
		return "", fmt.Errorf("failed to post message: %w", err)
	}

	s.log.Info("mcp tool result", "tool", "slack_post_message",
		"channel", resolvedChannel, "ts", ts)

	result := map[string]string{
		"channel":   resolvedChannel,
		"timestamp": ts,
		"status":    "posted",
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_reply_to_thread
// ---------------------------------------------------------------------------

func toolReplyToThread(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	threadTS, _ := args["thread_ts"].(string)
	text, _ := args["text"].(string)
	broadcast, _ := args["broadcast"].(bool)

	s.log.Info("mcp tool invoked", "tool", "slack_reply_to_thread",
		"channel", channel, "threadTS", threadTS, "textLen", len(text))

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || threadTS == "" || text == "" {
		return "", errors.New("'channel', 'thread_ts', and 'text' are all required")
	}

	resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
	if err != nil {
		return "", fmt.Errorf("failed to resolve channel %q: %w", channel, err)
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	}
	if broadcast {
		opts = append(opts, slack.MsgOptionBroadcast())
	}

	_, ts, err := api.PostMessageContext(ctx, resolvedChannel, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to post reply: %w", err)
	}

	s.log.Info("mcp tool result", "tool", "slack_reply_to_thread",
		"channel", resolvedChannel, "ts", ts)

	result := map[string]string{
		"channel":   resolvedChannel,
		"timestamp": ts,
		"thread_ts": threadTS,
		"status":    "replied",
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_add_reaction
// ---------------------------------------------------------------------------

func toolAddReaction(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	timestamp, _ := args["timestamp"].(string)
	emoji, _ := args["emoji"].(string)

	s.log.Info("mcp tool invoked", "tool", "slack_add_reaction",
		"channel", channel, "timestamp", timestamp, "emoji", emoji)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || timestamp == "" || emoji == "" {
		return "", errors.New("'channel', 'timestamp', and 'emoji' are all required")
	}

	// Strip colons if the caller includes them (e.g. ":thumbsup:" → "thumbsup")
	emoji = strings.Trim(emoji, ":")

	ref := slack.NewRefToMessage(channel, timestamp)
	if err := api.AddReactionContext(ctx, emoji, ref); err != nil {
		return "", fmt.Errorf("failed to add reaction: %w", err)
	}

	s.log.Info("mcp tool result", "tool", "slack_add_reaction", "status", "added")

	result := map[string]string{"status": "added", "emoji": emoji}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_remove_reaction
// ---------------------------------------------------------------------------

func toolRemoveReaction(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	timestamp, _ := args["timestamp"].(string)
	emoji, _ := args["emoji"].(string)

	s.log.Info("mcp tool invoked", "tool", "slack_remove_reaction",
		"channel", channel, "timestamp", timestamp, "emoji", emoji)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || timestamp == "" || emoji == "" {
		return "", errors.New("'channel', 'timestamp', and 'emoji' are all required")
	}

	emoji = strings.Trim(emoji, ":")

	ref := slack.NewRefToMessage(channel, timestamp)
	if err := api.RemoveReactionContext(ctx, emoji, ref); err != nil {
		return "", fmt.Errorf("failed to remove reaction: %w", err)
	}

	s.log.Info("mcp tool result", "tool", "slack_remove_reaction", "status", "removed")

	result := map[string]string{"status": "removed", "emoji": emoji}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_list_emoji
// ---------------------------------------------------------------------------

func toolListEmoji(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	nameFilter, _ := args["name_contains"].(string)
	nameFilter = strings.ToLower(nameFilter)

	s.log.Info("mcp tool invoked", "tool", "slack_list_emoji", "nameFilter", nameFilter)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	emojiMap, err := api.GetEmojiContext(ctx)
	if err != nil {
		return "", fmt.Errorf("Slack API error: %w", err)
	}

	emojis := filterEmoji(emojiMap, nameFilter)

	// Sort by name for deterministic output.
	slices.SortFunc(emojis, func(a, b emojiInfo) int { return strings.Compare(a.Name, b.Name) })

	s.log.Info("mcp tool result", "tool", "slack_list_emoji", "emojiCount", len(emojis))

	data, _ := json.MarshalIndent(emojis, "", "  ")
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_get_channel_history
// ---------------------------------------------------------------------------

func toolGetChannelHistory(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	oldest, _ := args["oldest"].(string)
	latest, _ := args["latest"].(string)

	limit := clampLimit(args["limit"], historyDefaultLimit, historyMaxLimit)

	s.log.Info("mcp tool invoked", "tool", "slack_get_channel_history",
		"channel", channel, "limit", limit)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" {
		return "", errors.New("'channel' is required")
	}

	resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
	if err != nil {
		return "", fmt.Errorf("failed to resolve channel %q: %w", channel, err)
	}

	params := &slack.GetConversationHistoryParameters{
		ChannelID:          resolvedChannel,
		Limit:              limit,
		Oldest:             oldest,
		Latest:             latest,
		IncludeAllMetadata: true,
	}

	resp, err := api.GetConversationHistoryContext(ctx, params)
	if err != nil {
		return "", fmt.Errorf("Slack API error: %w", err)
	}

	messages := make([]messageInfo, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		messages = append(messages, toMessageInfo(m.Msg, s.log))
	}

	s.log.Info("mcp tool result", "tool", "slack_get_channel_history", "messageCount", len(messages))

	data, _ := json.MarshalIndent(messages, "", "  ")
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_get_thread_replies
// ---------------------------------------------------------------------------

func toolGetThreadReplies(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	threadTS, _ := args["thread_ts"].(string)

	limit := clampLimit(args["limit"], threadRepliesDefaultLimit, threadRepliesMaxLimit)

	s.log.Info("mcp tool invoked", "tool", "slack_get_thread_replies",
		"channel", channel, "threadTS", threadTS, "limit", limit)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || threadTS == "" {
		return "", errors.New("'channel' and 'thread_ts' are required")
	}

	params := &slack.GetConversationRepliesParameters{
		ChannelID:          channel,
		Timestamp:          threadTS,
		Limit:              limit,
		IncludeAllMetadata: true,
	}

	msgs, _, _, err := api.GetConversationRepliesContext(ctx, params)
	if err != nil {
		return "", fmt.Errorf("Slack API error: %w", err)
	}

	messages := make([]messageInfo, 0, len(msgs))
	for _, m := range msgs {
		messages = append(messages, toMessageInfo(m.Msg, s.log))
	}

	s.log.Info("mcp tool result", "tool", "slack_get_thread_replies", "messageCount", len(messages))

	data, _ := json.MarshalIndent(messages, "", "  ")
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_list_users
// ---------------------------------------------------------------------------

func toolListUsers(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	nameFilter, _ := args["name_contains"].(string)
	nameFilter = strings.ToLower(nameFilter)
	includeBots, _ := args["include_bots"].(bool)

	limit := clampLimit(args["limit"], listUsersDefaultLimit, listUsersMaxLimit)

	s.log.Info("mcp tool invoked", "tool", "slack_list_users",
		"nameFilter", nameFilter, "limit", limit, "includeBots", includeBots)

	api, _, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	allUsers, err := api.GetUsersContext(ctx)
	if err != nil {
		return "", fmt.Errorf("Slack API error: %w", err)
	}

	var users []userInfo
	for _, u := range allUsers {
		info, ok := matchUser(u, nameFilter, includeBots)
		if !ok {
			continue
		}
		users = append(users, info)
		if len(users) >= limit {
			break
		}
	}

	s.log.Info("mcp tool result", "tool", "slack_list_users", "userCount", len(users))

	data, _ := json.MarshalIndent(users, "", "  ")
	return string(data), nil
}

// ---------------------------------------------------------------------------
// slack_upload_file
// ---------------------------------------------------------------------------

func toolUploadFile(ctx context.Context, s *mcpServer, args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	filename, _ := args["filename"].(string)
	filePath, _ := args["file_path"].(string)
	content, _ := args["content"].(string)
	title, _ := args["title"].(string)
	initialComment, _ := args["initial_comment"].(string)
	threadTS, _ := args["thread_ts"].(string)

	s.log.Info("mcp tool invoked", "tool", "slack_upload_file",
		"channel", channel, "filename", filename,
		"hasFilePath", filePath != "", "contentLen", len(content))

	api, token, err := s.slackClient(ctx)
	if err != nil {
		return "", err
	}

	if channel == "" || filename == "" {
		return "", errors.New("'channel' and 'filename' are required")
	}

	// Exactly one source must be provided.
	if filePath == "" && content == "" {
		return "", errors.New("one of 'file_path' or 'content' is required")
	}
	if filePath != "" && content != "" {
		return "", errors.New("provide only one of 'file_path' or 'content'")
	}

	resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
	if err != nil {
		return "", fmt.Errorf("failed to resolve channel %q: %w", channel, err)
	}

	if title == "" {
		title = filename
	}

	params := slack.UploadFileParameters{
		Filename:        filename,
		Title:           title,
		Channel:         resolvedChannel,
		InitialComment:  initialComment,
		ThreadTimestamp: threadTS,
	}

	// On the file_path branch we open the validated file ourselves and
	// hand a *os.File to the upload. Passing the path would re-open it
	// later, leaving a TOCTOU window where a symlink swap (or rename)
	// between validation and re-open could redirect the upload to a file
	// outside the upload directory. Streaming through the fd we already
	// pinned closes that window.
	if filePath != "" {
		f, size, errKind := openUploadPath(s.opts.UploadDir, filePath)
		if errKind != "" {
			s.log.Warn("mcp tool aborted", "tool", "slack_upload_file",
				"reason", errKind, "basename", filepath.Base(filePath))
			return "", errors.New(uploadPathUserMessage(errKind))
		}
		defer f.Close()
		params.Reader = f
		params.FileSize = int(size)
	} else {
		params.Content = content
		params.FileSize = len(content)
	}

	fileSummary, err := uploadSlackFileContext(ctx, api, token, params)
	if err != nil {
		return "", fmt.Errorf("failed to upload file: %w", err)
	}

	s.log.Info("mcp tool result", "tool", "slack_upload_file",
		"fileID", fileSummary.ID, "channel", resolvedChannel)

	result := map[string]string{
		"status":  "uploaded",
		"file_id": fileSummary.ID,
		"title":   fileSummary.Title,
		"channel": resolvedChannel,
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// ---------------------------------------------------------------------------
// Channel listing and resolution
// ---------------------------------------------------------------------------

// listChannelsDefaults centralizes the tunables for slack_list_channels so
// they're easy to find and reason about (and so tests can depend on the
// named constants instead of the raw numbers).
const (
	// listChannelsDefaultLimit is the default cap on returned channels when
	// the caller doesn't specify one.
	listChannelsDefaultLimit = 200
	// listChannelsMaxLimit clamps the caller-supplied limit. Anything larger
	// is silently reduced to this value.
	listChannelsMaxLimit = 1000
	// listChannelsPageSize is the per-request page size sent to Slack. 200 is
	// the Slack-recommended tier-2 page size.
	listChannelsPageSize = 200
	// listChannelsMaxPages caps pagination to avoid exhausting Slack Tier 2
	// rate limits in large workspaces that span many hundreds of channels.
	listChannelsMaxPages = 5
)

// listChannelsArgs is the parsed representation of slack_list_channels
// MCP tool arguments.
type listChannelsArgs struct {
	Limit      int
	NameFilter string // already lowercased
	MemberOnly bool
}

// parseListChannelsArgs converts the loose MCP argument map into a typed
// struct, applying defaults and clamping the limit.
func parseListChannelsArgs(args map[string]any) listChannelsArgs {
	out := listChannelsArgs{
		Limit:      listChannelsDefaultLimit,
		MemberOnly: true,
	}
	if v, ok := args["limit"].(float64); ok && v > 0 {
		// Clamp before converting: a float beyond the int range converts to
		// an unspecified value (negative on some platforms), which would
		// slip past the clamp and panic when the result is truncated.
		if v > listChannelsMaxLimit {
			out.Limit = listChannelsMaxLimit
		} else {
			out.Limit = int(v)
		}
	}
	if v, ok := args["name_contains"].(string); ok {
		out.NameFilter = strings.ToLower(v)
	}
	if v, ok := args["member_only"].(bool); ok {
		out.MemberOnly = v
	}
	return out
}

// channelInfo is the JSON shape returned by slack_list_channels.
type channelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Topic      string `json:"topic,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	NumMembers int    `json:"numMembers"`
	IsMember   bool   `json:"isMember"`
}

// matchChannel returns (info, true) if ch passes the filters in opts, or
// (zero, false) otherwise. Pure on its inputs — no I/O.
func matchChannel(ch slack.Channel, opts listChannelsArgs) (channelInfo, bool) {
	if opts.MemberOnly && !ch.IsMember {
		return channelInfo{}, false
	}
	if opts.NameFilter != "" && !strings.Contains(strings.ToLower(ch.Name), opts.NameFilter) {
		return channelInfo{}, false
	}
	return channelInfo{
		ID:         ch.ID,
		Name:       ch.Name,
		Topic:      ch.Topic.Value,
		Purpose:    ch.Purpose.Value,
		NumMembers: ch.NumMembers,
		IsMember:   ch.IsMember,
	}, true
}

// slackConversationLister is the small subset of slack.Client that the
// channel-listing logic needs. Declared here so tests can inject a fake.
type slackConversationLister interface {
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
}

// listSlackChannels paginates through Slack's conversations.list API and
// applies opts. It is wired up to a real *slack.Client in production, but
// tests can inject any slackConversationLister.
//
// Partial-success semantics: if we've already collected at least one channel
// and a subsequent page fails, the error is reported to the caller (logged)
// and the channels collected so far are returned.
func listSlackChannels(
	ctx context.Context,
	api slackConversationLister,
	opts listChannelsArgs,
	onPartial func(err error, collected int),
) ([]channelInfo, error) {
	var channels []channelInfo
	cursor := ""
	for page := 0; page < listChannelsMaxPages; page++ {
		params := &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			Limit:           listChannelsPageSize,
			Cursor:          cursor,
			ExcludeArchived: true,
		}
		chs, nextCursor, err := api.GetConversationsContext(ctx, params)
		if err != nil {
			if len(channels) > 0 {
				if onPartial != nil {
					onPartial(err, len(channels))
				}
				break
			}
			return nil, err
		}
		for _, ch := range chs {
			info, ok := matchChannel(ch, opts)
			if !ok {
				continue
			}
			channels = append(channels, info)
			if len(channels) >= opts.Limit {
				break
			}
		}
		if len(channels) >= opts.Limit || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	if len(channels) > opts.Limit {
		channels = channels[:opts.Limit]
	}
	return channels, nil
}

// resolveSlackChannel converts a #channel-name to a channel ID.
// If the input already looks like a conversation ID (starts with C/G/D),
// it's returned as-is. User IDs (U/W prefix) are NOT accepted here —
// chat.postMessage expects a conversation ID, not a user ID, so passing a
// user ID would fail at Slack. Callers who want to DM a user must open a
// conversation first and pass the resulting D... ID.
func resolveSlackChannel(ctx context.Context, api *slack.Client, channel string) (string, error) {
	channel = strings.TrimPrefix(channel, "#")

	if len(channel) >= 9 && !strings.Contains(channel, " ") {
		switch channel[0] {
		case 'C', 'G', 'D':
			return channel, nil
		}
	}

	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Types:  []string{"public_channel", "private_channel"},
			Limit:  200,
			Cursor: cursor,
		}
		chs, nextCursor, err := api.GetConversationsContext(ctx, params)
		if err != nil {
			return "", fmt.Errorf("list channels: %w", err)
		}
		for _, ch := range chs {
			if ch.Name == channel {
				return ch.ID, nil
			}
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return "", fmt.Errorf("channel %q not found", channel)
}

// ---------------------------------------------------------------------------
// Result shapes and limits
// ---------------------------------------------------------------------------

// emojiInfo is the JSON shape returned by slack_list_emoji.
type emojiInfo struct {
	Name  string `json:"name"`
	Value string `json:"value"` // URL or "alias:name"
}

const (
	historyDefaultLimit = 20
	historyMaxLimit     = 100
)

// messageInfo is the JSON shape returned by slack_get_channel_history and
// slack_get_thread_replies. Fields beyond the bare message text are included
// so the agent can see files, reactions, rich-layout blocks, edits, and
// bot-posted messages.
type messageInfo struct {
	// User and Text intentionally do NOT use omitempty: these keys were
	// always present in the prior shape and consumers may rely on the key
	// existing (with an empty string for bot/system messages).
	User        string              `json:"user"`
	Username    string              `json:"username,omitempty"`
	BotID       string              `json:"bot_id,omitempty"`
	AppID       string              `json:"app_id,omitempty"`
	Text        string              `json:"text"`
	Timestamp   string              `json:"timestamp"`
	ThreadTS    string              `json:"thread_ts,omitempty"`
	ReplyCount  int                 `json:"reply_count,omitempty"`
	SubType     string              `json:"subtype,omitempty"`
	Edited      *messageEdited      `json:"edited,omitempty"`
	Files       []messageFile       `json:"files,omitempty"`
	Reactions   []messageReaction   `json:"reactions,omitempty"`
	Attachments []messageAttachment `json:"attachments,omitempty"`
	// Blocks is the re-serialized form of slack-go's Blocks struct (not the
	// raw Slack API JSON). Unknown / future block types that slack-go cannot
	// decode will be missing.
	Blocks   json.RawMessage  `json:"blocks,omitempty"`
	Metadata *messageMetadata `json:"metadata,omitempty"`
}

type messageEdited struct {
	User      string `json:"user,omitempty"`
	Timestamp string `json:"ts,omitempty"`
}

// messageFile is a slimmed-down view of a Slack file shared in a message.
// We deliberately drop the dozen-plus thumbnail fields and keep only the
// metadata an agent needs to identify and (optionally) fetch the file.
type messageFile struct {
	ID                 string `json:"id,omitempty"`
	Name               string `json:"name,omitempty"`
	Title              string `json:"title,omitempty"`
	Mimetype           string `json:"mimetype,omitempty"`
	Filetype           string `json:"filetype,omitempty"`
	PrettyType         string `json:"pretty_type,omitempty"`
	Size               int    `json:"size,omitempty"`
	URLPrivate         string `json:"url_private,omitempty"`
	URLPrivateDownload string `json:"url_private_download,omitempty"`
	Permalink          string `json:"permalink,omitempty"`
	PermalinkPublic    string `json:"permalink_public,omitempty"`
	User               string `json:"user,omitempty"`
}

type messageReaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users,omitempty"`
}

// messageAttachment trims slack.Attachment to the fields agents typically
// need (text+layout, not unfurl-specific thumbnail dimensions).
type messageAttachment struct {
	Color       string                   `json:"color,omitempty"`
	Fallback    string                   `json:"fallback,omitempty"`
	Title       string                   `json:"title,omitempty"`
	TitleLink   string                   `json:"title_link,omitempty"`
	Pretext     string                   `json:"pretext,omitempty"`
	Text        string                   `json:"text,omitempty"`
	AuthorName  string                   `json:"author_name,omitempty"`
	AuthorLink  string                   `json:"author_link,omitempty"`
	ServiceName string                   `json:"service_name,omitempty"`
	FromURL     string                   `json:"from_url,omitempty"`
	ImageURL    string                   `json:"image_url,omitempty"`
	ThumbURL    string                   `json:"thumb_url,omitempty"`
	Footer      string                   `json:"footer,omitempty"`
	Fields      []messageAttachmentField `json:"fields,omitempty"`
	Blocks      json.RawMessage          `json:"blocks,omitempty"`
}

type messageAttachmentField struct {
	Title string `json:"title,omitempty"`
	Value string `json:"value,omitempty"`
	Short bool   `json:"short,omitempty"`
}

type messageMetadata struct {
	EventType    string         `json:"event_type,omitempty"`
	EventPayload map[string]any `json:"event_payload,omitempty"`
}

// toMessageInfo flattens a slack.Msg into the agent-facing messageInfo shape.
// Empty slices/pointers stay nil so json.Marshal omits them via omitempty.
// logger may be nil; if non-nil, block-marshal failures are reported as warnings
// rather than silently dropping the field.
func toMessageInfo(m slack.Msg, logger *slog.Logger) messageInfo {
	info := messageInfo{
		User:       m.User,
		Username:   m.Username,
		BotID:      m.BotID,
		Text:       m.Text,
		Timestamp:  m.Timestamp,
		ThreadTS:   m.ThreadTimestamp,
		ReplyCount: m.ReplyCount,
		SubType:    m.SubType,
	}
	if m.BotProfile != nil {
		info.AppID = m.BotProfile.AppID
	}
	if m.Edited != nil {
		info.Edited = &messageEdited{User: m.Edited.User, Timestamp: m.Edited.Timestamp}
	}
	if len(m.Files) > 0 {
		info.Files = make([]messageFile, 0, len(m.Files))
		for _, f := range m.Files {
			info.Files = append(info.Files, messageFile{
				ID:                 f.ID,
				Name:               f.Name,
				Title:              f.Title,
				Mimetype:           f.Mimetype,
				Filetype:           f.Filetype,
				PrettyType:         f.PrettyType,
				Size:               f.Size,
				URLPrivate:         f.URLPrivate,
				URLPrivateDownload: f.URLPrivateDownload,
				Permalink:          f.Permalink,
				PermalinkPublic:    f.PermalinkPublic,
				User:               f.User,
			})
		}
	}
	if len(m.Reactions) > 0 {
		info.Reactions = make([]messageReaction, 0, len(m.Reactions))
		for _, r := range m.Reactions {
			info.Reactions = append(info.Reactions, messageReaction{
				Name:  r.Name,
				Count: r.Count,
				Users: r.Users,
			})
		}
	}
	if len(m.Attachments) > 0 {
		info.Attachments = make([]messageAttachment, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			att := messageAttachment{
				Color:       a.Color,
				Fallback:    a.Fallback,
				Title:       a.Title,
				TitleLink:   a.TitleLink,
				Pretext:     a.Pretext,
				Text:        a.Text,
				AuthorName:  a.AuthorName,
				AuthorLink:  a.AuthorLink,
				ServiceName: a.ServiceName,
				FromURL:     a.FromURL,
				ImageURL:    a.ImageURL,
				ThumbURL:    a.ThumbURL,
				Footer:      a.Footer,
			}
			if len(a.Fields) > 0 {
				att.Fields = make([]messageAttachmentField, 0, len(a.Fields))
				for _, f := range a.Fields {
					att.Fields = append(att.Fields, messageAttachmentField{
						Title: f.Title, Value: f.Value, Short: f.Short,
					})
				}
			}
			if len(a.Blocks.BlockSet) > 0 {
				if b, err := json.Marshal(a.Blocks); err == nil {
					att.Blocks = b
				} else if logger != nil {
					logger.Warn("failed to marshal attachment blocks", "ts", m.Timestamp, "err", err)
				}
			}
			info.Attachments = append(info.Attachments, att)
		}
	}
	if len(m.Blocks.BlockSet) > 0 {
		if b, err := json.Marshal(m.Blocks); err == nil {
			info.Blocks = b
		} else if logger != nil {
			logger.Warn("failed to marshal message blocks", "ts", m.Timestamp, "err", err)
		}
	}
	if m.Metadata.EventType != "" || len(m.Metadata.EventPayload) > 0 {
		info.Metadata = &messageMetadata{
			EventType:    m.Metadata.EventType,
			EventPayload: m.Metadata.EventPayload,
		}
	}
	return info
}

const (
	threadRepliesDefaultLimit = 50
	threadRepliesMaxLimit     = 200
)

const (
	listUsersDefaultLimit = 200
	listUsersMaxLimit     = 500
)

// userInfo is the JSON shape returned by slack_list_users.
type userInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	RealName    string `json:"realName"`
	DisplayName string `json:"displayName"`
	IsBot       bool   `json:"isBot,omitempty"`
	IsAdmin     bool   `json:"isAdmin,omitempty"`
	Deleted     bool   `json:"deleted,omitempty"`
}

// ---------------------------------------------------------------------------
// File upload
// ---------------------------------------------------------------------------

// uploadSlackFileContext mirrors slack-go's three-step external upload while
// using a cancellation-safe multipart writer for the data transfer. slack-go
// v0.21.0 uses an unbuffered producer error channel: if the source stream fails
// (for example because reading the file fails part-way), its producer can block
// before closing the pipe and leave both the upload and goroutine stuck.
func uploadSlackFileContext(ctx context.Context, api *slack.Client, token string, params slack.UploadFileParameters) (*slack.FileSummary, error) {
	if params.Filename == "" {
		return nil, fmt.Errorf("file.upload.v2: filename cannot be empty")
	}
	if params.FileSize == 0 {
		return nil, fmt.Errorf("file.upload.v2: file size cannot be 0")
	}

	upload, err := api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{
		AltTxt:      params.AltTxt,
		FileName:    params.Filename,
		FileSize:    params.FileSize,
		SnippetType: params.SnippetType,
	})
	if err != nil {
		return nil, fmt.Errorf("GetUploadURLExternal: %w", err)
	}

	var source io.Reader
	if params.Reader != nil {
		source = params.Reader
	} else {
		source = strings.NewReader(params.Content)
	}
	if err := postSlackMultipartFile(ctx, http.DefaultClient, upload.UploadURL, token, params.Filename, source); err != nil {
		return nil, fmt.Errorf("UploadToURL: %w", err)
	}

	completed, err := api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{
		Files:           []slack.FileSummary{{ID: upload.FileID, Title: params.Title}},
		Channel:         params.Channel,
		InitialComment:  params.InitialComment,
		ThreadTimestamp: params.ThreadTimestamp,
		Blocks:          params.Blocks,
	})
	if err != nil {
		return nil, fmt.Errorf("CompleteUploadExternal: %w", err)
	}
	if len(completed.Files) != 1 {
		return nil, fmt.Errorf("file.upload.v2: something went wrong; received %d files instead of 1", len(completed.Files))
	}
	return &completed.Files[0], nil
}

// postSlackMultipartFile streams one file to Slack's pre-signed upload URL.
// The producer result is buffered and every failure path closes both ends that
// may be blocking, so a source/read failure or request cancellation cannot
// strand the multipart goroutine.
func postSlackMultipartFile(ctx context.Context, client *http.Client, uploadURL, token, filename string, source io.Reader) error {
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	producerDone := make(chan error, 1)

	closeSource := func() {
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	abort := func(err error) {
		_ = pipeReader.CloseWithError(err)
		_ = pipeWriter.CloseWithError(err)
		closeSource()
	}

	go func() {
		part, err := multipartWriter.CreateFormFile("file", filename)
		if err == nil {
			_, err = io.Copy(part, source)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
		} else {
			_ = pipeWriter.Close()
		}
		producerDone <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, pipeReader)
	if err != nil {
		abort(err)
		return err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		abort(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("upload returned HTTP %d", resp.StatusCode)
		abort(err)
		_, _ = io.Copy(io.Discard, resp.Body)
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	select {
	case err := <-producerDone:
		if err != nil {
			abort(err)
			return err
		}
		return nil
	case <-ctx.Done():
		abort(ctx.Err())
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Shared helpers (extracted for testability)
// ---------------------------------------------------------------------------

// clampLimit reads a JSON-decoded numeric "limit" argument and clamps it to
// [1, max]. Inputs are floats because encoding/json decodes JSON numbers as float64.
//
// Returns def when the argument is missing, non-numeric, non-positive, or
// rounds down to zero (e.g. 0.5).
func clampLimit(raw any, def, max int) int {
	v, ok := raw.(float64)
	if !ok || v <= 0 {
		return def
	}
	n := int(v)
	if n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// filterEmoji returns emojiInfo entries from the Slack emoji map whose name
// contains nameFilter (case-insensitive). An empty filter matches all.
func filterEmoji(emojiMap map[string]string, nameFilter string) []emojiInfo {
	filter := strings.ToLower(nameFilter)
	var emojis []emojiInfo
	for name, value := range emojiMap {
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		emojis = append(emojis, emojiInfo{Name: name, Value: value})
	}
	return emojis
}

// matchUser applies the slack_list_users filter rules to a Slack user and
// returns its userInfo projection on match. nameFilter is matched
// case-insensitively against Name, RealName, and DisplayName; it does not
// need to be pre-lowercased by the caller.
//
// Rules (in order):
//  1. Skip users marked Deleted.
//  2. Skip bot users (IsBot or USLACKBOT) unless includeBots is true.
//  3. If nameFilter is non-empty, the user's Name, RealName, or DisplayName
//     must contain it as a case-insensitive substring.
func matchUser(u slack.User, nameFilter string, includeBots bool) (userInfo, bool) {
	if u.Deleted {
		return userInfo{}, false
	}
	if (u.IsBot || u.ID == "USLACKBOT") && !includeBots {
		return userInfo{}, false
	}
	if nameFilter != "" {
		needle := strings.ToLower(nameFilter)
		match := strings.Contains(strings.ToLower(u.Name), needle) ||
			strings.Contains(strings.ToLower(u.RealName), needle) ||
			strings.Contains(strings.ToLower(u.Profile.DisplayName), needle)
		if !match {
			return userInfo{}, false
		}
	}
	return userInfo{
		ID:          u.ID,
		Name:        u.Name,
		RealName:    u.RealName,
		DisplayName: u.Profile.DisplayName,
		IsBot:       u.IsBot,
		IsAdmin:     u.IsAdmin,
	}, true
}

// upload path validation error kinds. These are constants (not formatted
// errors) so they can be safely logged without leaking absolute paths or
// other host details.
const (
	uploadErrEmpty    = "empty"
	uploadErrInvalid  = "invalid_path"
	uploadErrNotFound = "not_found"
	uploadErrOutside  = "outside_upload_dir"
	uploadErrIsDir    = "is_directory"
	uploadErrNotFile  = "not_regular_file"
	uploadErrOpenFail = "open_failed"
	uploadErrStatFail = "stat_failed"
	uploadErrSwapped  = "swapped_during_validation"
)

// uploadPathUserMessage maps an internal error kind to a fixed,
// path-free user-facing message.
func uploadPathUserMessage(kind string) string {
	switch kind {
	case uploadErrEmpty:
		return "'file_path' must not be empty"
	case uploadErrOutside:
		return "'file_path' must be inside the kojo upload directory"
	case uploadErrIsDir:
		return "'file_path' must be a regular file, not a directory"
	case uploadErrNotFile:
		return "'file_path' must be a regular file"
	case uploadErrNotFound:
		return "'file_path' does not exist or is not accessible"
	case uploadErrSwapped:
		return "'file_path' changed during validation; refusing to upload"
	default:
		return "'file_path' is invalid"
	}
}

// openUploadPath validates that p points to a regular file inside
// uploadDir (the kojo upload directory) and returns an *os.File already
// opened on the validated inode. Callers MUST Close the returned file.
//
// Opening up-front (rather than just resolving a path string) eliminates
// the TOCTOU window between validation and a later re-open: even if a
// symlink/rename races our check, the upload reads from the fd we already
// pinned to the verified inode.
//
// On error, returns (nil, 0, kind) where kind is one of the uploadErr*
// constants — never a formatted error containing a host path.
func openUploadPath(uploadDir, p string) (*os.File, int64, string) {
	if p == "" {
		return nil, 0, uploadErrEmpty
	}
	// Without a known upload directory nothing can be inside it. This must
	// be decided here: an empty root would make the prefix check below
	// accept every absolute path.
	if uploadDir == "" {
		return nil, 0, uploadErrOutside
	}

	// Canonicalize the upload root once (handles /tmp → /private/tmp on macOS).
	canonicalRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		canonicalRoot = uploadDir
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, 0, uploadErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, 0, uploadErrNotFound
	}

	// Must be inside the upload directory (canonical-prefix check).
	if resolved != canonicalRoot &&
		!strings.HasPrefix(resolved, canonicalRoot+string(filepath.Separator)) {
		return nil, 0, uploadErrOutside
	}

	// Open the resolved (canonical) path.
	f, err := os.Open(resolved)
	if err != nil {
		return nil, 0, uploadErrOpenFail
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, uploadErrStatFail
	}
	if info.IsDir() {
		f.Close()
		return nil, 0, uploadErrIsDir
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrNotFile
	}

	// Close the residual TOCTOU window between EvalSymlinks and Open: if
	// the path (or any of its parents) was swapped to a symlink or to a
	// different inode in that gap, reject the upload.
	//
	// Three checks together cover the realistic attack vectors:
	//  1. Re-resolve symlinks: if any component was made a symlink after
	//     the first EvalSymlinks, the canonical form changes.
	//  2. Lstat regular-file: reject if the final component is now a
	//     symlink or non-regular type.
	//  3. SameFile inode match: reject if the path now points to a
	//     different inode than the open fd.
	reResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil || reResolved != resolved {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	lstatInfo, err := os.Lstat(resolved)
	if err != nil {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !lstatInfo.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !os.SameFile(info, lstatInfo) {
		f.Close()
		return nil, 0, uploadErrSwapped
	}

	return f, info.Size(), ""
}
