package agent

import "errors"

// ErrMsgCancelled is the error message attached to a "done" event when
// the backend process is terminated due to a user-initiated cancellation
// (context.Canceled) rather than a deadline.
const ErrMsgCancelled = "cancelled: process was terminated"

// KeyedStopRequestedReason is the close reason of a session stopped through
// the background-sessions API. Surfaces render it as an explicit stop.
const KeyedStopRequestedReason = "エージェントの依頼で停止"

// KeyedUserStopReason is the close reason of a session whose background tasks
// a person in the thread stopped (Slack `!stop all`). Unlike an agent-requested
// stop, the agent gets a one-time note on its next turn in the thread.
const KeyedUserStopReason = "ユーザーの依頼で停止"

var (
	// ErrSteerUnsupported is returned by SteerOneShot when the target turn
	// is running on a backend that doesn't support mid-turn steering.
	ErrSteerUnsupported = errors.New("steering is not supported for this backend")

	// ErrAgentNotBusy is returned by SteerOneShot when there is no turn
	// currently running to steer.
	ErrAgentNotBusy = errors.New("agent has no turn in progress")

	// ErrSteerDeliveryUncertain means a steer may have reached the backend or
	// remote holder before its acknowledgement was lost. Callers must not
	// replay the message as a fresh turn: the model may have observed it.
	ErrSteerDeliveryUncertain = errors.New("steer delivery outcome is uncertain")

	// ErrInvalidQuestionAnswer rejects incomplete or malformed answer payloads.
	ErrInvalidQuestionAnswer = errors.New("invalid question answers")

	// ErrQuestionNotFound is returned by AnswerOneShotQuestion when the
	// given requestID does not match any pending user_question on the
	// running turn (already answered, expired, or never existed).
	ErrQuestionNotFound = errors.New("no pending question with that request id")

	// ErrBackgroundSessionNotFound is returned by StopThreadBackgroundTasks
	// when the thread has no lingering session with background tasks.
	ErrBackgroundSessionNotFound = errors.New("background session not found")
)
