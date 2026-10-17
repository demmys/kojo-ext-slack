package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/loppo-llc/kojo-ext-slack/internal/chathistory"
)

// SlackBotConfig holds Slack bot configuration for an agent.
type SlackBotConfig struct {
	Enabled       bool `json:"enabled"`
	ThreadReplies bool `json:"threadReplies"` // always reply in-thread (default true)

	// Reaction patterns — which message types the bot responds to.
	// All default to true for backwards compatibility.
	RespondDM      *bool `json:"respondDM,omitempty"`      // respond to direct messages
	RespondMention *bool `json:"respondMention,omitempty"` // respond to @mentions in channels
	RespondThread  *bool `json:"respondThread,omitempty"`  // auto-reply in threads with history
}

// ReactDM returns whether the bot should respond to direct messages.
func (c *SlackBotConfig) ReactDM() bool { return c.RespondDM == nil || *c.RespondDM }

// ReactMention returns whether the bot should respond to @mentions.
func (c *SlackBotConfig) ReactMention() bool { return c.RespondMention == nil || *c.RespondMention }

// ReactThread returns whether the bot should auto-reply in threads with history.
func (c *SlackBotConfig) ReactThread() bool { return c.RespondThread == nil || *c.RespondThread }

// MessageAttachment represents a file attached to a chat message.
type MessageAttachment struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Mime   string `json:"mime"`
	PeerID string `json:"peerId,omitempty"` // host that owns Path; prevents handoff races
}

// Message represents a single chat message in the transcript.
type Message struct {
	ID          string              `json:"id"`
	Role        string              `json:"role"` // "user", "assistant", "system"
	Content     string              `json:"content"`
	Thinking    string              `json:"thinking,omitempty"` // intermediate reasoning
	ToolUses    []ToolUse           `json:"toolUses,omitempty"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	Timestamp   string              `json:"timestamp"` // RFC3339
	Usage       *Usage              `json:"usage,omitempty"`
}

// ToolUse represents a single tool invocation within a message.
type ToolUse struct {
	ID     string `json:"id,omitempty"` // tool call ID for matching results
	Name   string `json:"name"`
	Input  string `json:"input"`  // JSON string
	Output string `json:"output"` // truncated output
	// Text holds a subagent narrative snippet when this entry is a
	// synthetic "text bubble" child rather than a real tool call.
	Text string `json:"text,omitempty"`
	// Children holds tool calls emitted by a subagent spawned via the Task
	// tool, when this ToolUse is that Task invocation.
	Children []ToolUse `json:"children,omitempty"`
}

// Usage tracks token consumption for a message.
type Usage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CostUSD                  float64 `json:"costUSD,omitempty"`
}

// ChatEvent is one event of a turn's stream, as kojo encodes it on the
// ext-chat NDJSON wire. Core fields the bot never reads (the Codex goal
// snapshot, the rate-limit snapshot) are left out; unknown JSON fields are
// ignored on decode.
type ChatEvent struct {
	Type         string              `json:"type"` // "status", "text", "thinking", "tool_use", "tool_result", "done", "error", "message", "attachment"
	Status       string              `json:"status,omitempty"`
	Delta        string              `json:"delta,omitempty"`
	ToolUseID    string              `json:"toolUseId,omitempty"`
	ToolName     string              `json:"toolName,omitempty"`
	ToolInput    string              `json:"toolInput,omitempty"`
	ToolOutput   string              `json:"toolOutput,omitempty"`
	Message      *Message            `json:"message,omitempty"`
	Attachments  []MessageAttachment `json:"attachments,omitempty"`
	Usage        *Usage              `json:"usage,omitempty"`
	ErrorMessage string              `json:"errorMessage,omitempty"`
	// ErrorCode preserves the backend classification separately from human-readable text.
	ErrorCode string `json:"errorCode,omitempty"`
	// ParentToolUseID is set when this event originates from a subagent
	// spawned by a Task tool call rather than the main assistant turn.
	// Consumers that accumulate the main turn's text/toolUses MUST skip
	// events carrying a non-empty ParentToolUseID.
	ParentToolUseID string `json:"parentToolUseId,omitempty"`
	// RequestID identifies a control_request the CLI raised for an
	// interactive tool (AskUserQuestion or Codex request_user_input). Set only
	// on question lifecycle events.
	RequestID string `json:"requestId,omitempty"`
	// Questions carries the shared UserQuestion array. Only populated on
	// "user_question" events.
	Questions json.RawMessage `json:"questions,omitempty"`
	// nil preserves Claude/older-peer blocking semantics.
	QuestionBlocking *bool `json:"questionBlocking,omitempty"`
	// BackgroundTasksPending is set on a keyed turn's terminal "done" when the
	// CLI process lingers because this many background tasks are still
	// running; their completion arrives later as a keyed background turn.
	BackgroundTasksPending int `json:"backgroundTasksPending,omitempty"`
	// SteeredIntoBackground marks a keyed turn's synthetic "done" when its
	// user message was steered into a running background notification turn
	// instead (the reply arrives via the keyed background handler). Surfaces
	// must post nothing for it.
	SteeredIntoBackground bool `json:"steeredIntoBackground,omitempty"`
}

// OneShotOpts configures a ChatOneShot invocation. It carries the subset of
// kojo's options the Slack bot sets; the HTTP client maps each field onto the
// ext-chat request.
type OneShotOpts struct {
	GoalUserID string // initiating Slack user, retained for recovery question ownership
	Goal       *GoalRequest
	// InteractiveQuestions opts in only when the response surface can answer.
	InteractiveQuestions bool

	// PreserveTerminalOnCancel keeps a cancelled terminal event pending until
	// the response adapter consumes it. Slack enables this because its live text
	// deltas are intentionally lossy and the terminal message is authoritative.
	PreserveTerminalOnCancel bool

	// SessionKey selects the resumable backend session of one Slack
	// conversation: "<agentID>:slack:<channel>:<threadTS>".
	SessionKey string

	// ExpectedHolderPeer fences a queued post-handoff turn to the holder that
	// issued the callback. kojo rejects the turn if another handoff wins
	// before this FIFO reservation reaches execution.
	ExpectedHolderPeer string
	// HandoffArrivalReservation is a response-adapter-owned FIFO slot reserved
	// immediately behind this turn. kojo activates it if the turn moves
	// devices, or the adapter releases it when the turn ends.
	HandoffArrivalReservation HandoffArrivalReservation

	// ForceFreshSession starts a new backend session instead of resuming the
	// one stored for SessionKey; History is injected into it.
	ForceFreshSession bool

	// History is the response surface's canonical transcript, excluding the
	// current user message.
	History []chathistory.HistoryMessage

	// HistorySelfUserID identifies this agent's messages inside History.
	HistorySelfUserID string

	// SystemPromptExtra is appended to the system prompt for this call only.
	SystemPromptExtra string

	// DisableKojoAttachmentInstructions hides kojo's response-attachment
	// staging instructions. ext-chat always disables them, so the field has
	// no wire counterpart; it is kept because the bot sets it.
	DisableKojoAttachmentInstructions bool

	// Attachments are files already written into KOJO_UPLOAD_DIR.
	Attachments []MessageAttachment

	// LingerBackgroundTasks opts a keyed turn into keeping its CLI process
	// alive after the result while run_in_background tasks are still pending,
	// so their completion is delivered as a keyed background turn.
	LingerBackgroundTasks bool
}

// HandoffArrivalReservation is the FIFO slot behind a turn that kojo
// activates when the turn hands the agent off to another device.
type HandoffArrivalReservation interface {
	Activate(ctx context.Context, prompt, expectedHolder string) error
	Release()
}

// ErrStoppedByUser is the cancel cause (context.WithCancelCause) of a turn
// a person stopped with `!stop`; kojo is then told the user stopped it.
var ErrStoppedByUser = errors.New("turn stopped by the user")

// KeyedBackgroundHandler is implemented by a response surface (the Slack bot)
// that owns keyed conversations of an agent. It receives unsolicited turns
// produced by a keyed lingering session — the completion notification of a
// run_in_background task started by an earlier thread turn.
type KeyedBackgroundHandler interface {
	// HandleKeyedBackgroundTurn must consume events until the channel closes.
	// The stream has the same contract as a ChatOneShot result (deltas, then
	// a single terminal done/error); a done carrying BackgroundTasksPending>0
	// means more tasks are still running on the lingering process.
	// cancel aborts the background turn; byUser marks an explicit stop by a
	// person (a `!stop`), which kojo reports to the agent differently from
	// a stop the bot made.
	HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, cancel func(byUser bool))
	// KeyedBackgroundTasksAbandoned reports that the keyed session exited
	// while `pending` background tasks were still running (best-effort
	// notice; their completion will never arrive).
	KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string)
}
