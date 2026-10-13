package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrGoalOwnerForbidden mirrors kojo's sentinel. The HTTP client returns it
// for the `goal_owner_forbidden` code so the bot's errors.Is checks hold.
var ErrGoalOwnerForbidden = errors.New("only the user who started this goal can control or reply to it")

// codexThreadIDPattern matches Codex app-server thread IDs (UUIDv7-shaped
// lowercase hex). ParseGoalCommand uses it to fence recovery commands.
var codexThreadIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isCodexThreadID(s string) bool {
	return codexThreadIDPattern.MatchString(s)
}

// GoalRequest is explicit user intent, never extracted from model output or
// historical context. Controls use the same authenticated conversation route.
type GoalRequest struct {
	ExpectedHandoffID  string `json:"expectedHandoffId,omitempty"`
	ExpectedRunID      string `json:"expectedRunId,omitempty"`
	OperationID        string `json:"operationId,omitempty"`
	ExpectedGeneration *int64 `json:"expectedGeneration,omitempty"`
	ExpectedThreadID   string `json:"expectedThreadId,omitempty"`
	Action             string `json:"action"`
	Objective          string `json:"objective,omitempty"`
	TokenBudget        *int64 `json:"tokenBudget,omitempty"`
}

func ParseGoalCommand(text string) (*GoalRequest, error) {
	text = strings.TrimSpace(text)
	if text != "!goal" && !strings.HasPrefix(text, "!goal ") && !strings.HasPrefix(text, "!goal\n") {
		return nil, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, "!goal"))
	r := &GoalRequest{Action: "start", Objective: rest}
	switch rest {
	case "", "status":
		r.Action = "status"
		r.Objective = ""
	case "pause", "resume", "clear":
		r.Action = rest
		r.Objective = ""
	}
	if strings.HasPrefix(rest, "start ") {
		r.Objective = strings.TrimSpace(strings.TrimPrefix(rest, "start "))
	}
	if strings.HasPrefix(rest, "budget ") {
		r.Action = "budget"
		r.Objective = ""
		var n int64
		if _, err := fmt.Sscanf(rest, "budget %d", &n); err != nil {
			return nil, errors.New("usage: !goal budget <positive token limit>")
		}
		if fmt.Sprintf("budget %d", n) != rest {
			return nil, errors.New("usage: !goal budget <positive token limit>")
		}
		r.TokenBudget = &n
	}
	if strings.HasPrefix(rest, "--tokens ") {
		budgetAndGoal := strings.TrimSpace(strings.TrimPrefix(rest, "--tokens "))
		budget, objective, ok := strings.Cut(budgetAndGoal, " ")
		n, err := strconv.ParseInt(budget, 10, 64)
		if !ok || err != nil {
			return nil, errors.New("usage: !goal --tokens N <objective>")
		}
		r.Objective = strings.TrimSpace(objective)
		r.TokenBudget = &n
	}
	if strings.HasPrefix(rest, "resume-if ") {
		fields := strings.Fields(rest)
		if len(fields) != 3 && len(fields) != 4 && len(fields) != 5 {
			return nil, errors.New("invalid recovery command")
		}
		tid := fields[1]
		gen, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || !isCodexThreadID(tid) || gen < 0 {
			return nil, errors.New("invalid recovery command")
		}
		r.Action = "resume"
		r.Objective = ""
		r.ExpectedThreadID = tid
		r.ExpectedGeneration = &gen
		if len(fields) >= 4 && fields[3] != "-" {
			r.ExpectedRunID = fields[3]
		}
		if len(fields) == 5 {
			r.ExpectedHandoffID = fields[4]
		}
	}
	return r, r.Validate()
}
func (r *GoalRequest) Validate() error {
	if r == nil {
		return nil
	}
	if r.ExpectedHandoffID != "" && (r.ExpectedGeneration == nil || !isCodexThreadID(r.ExpectedHandoffID)) {
		return errors.New("invalid goal handoff identity")
	}
	if len(r.OperationID) > 256 {
		return errors.New("goal operation id too long")
	}
	switch r.Action {
	case "start":
		if strings.TrimSpace(r.Objective) == "" || utf8.RuneCountInString(r.Objective) > 4000 {
			return errors.New("goal objective must contain 1–4000 characters")
		}
	case "status", "pause", "resume", "clear", "budget":
		if r.Objective != "" {
			return errors.New("only start accepts a goal objective")
		}
	default:
		return errors.New("unknown goal action")
	}
	if r.ExpectedGeneration != nil && (r.Action != "resume" || *r.ExpectedGeneration < 0 || !isCodexThreadID(r.ExpectedThreadID)) {
		return errors.New("invalid recovery fence")
	}
	if r.ExpectedRunID != "" && (r.ExpectedGeneration == nil || len(r.ExpectedRunID) != 64 || strings.Trim(r.ExpectedRunID, "0123456789abcdef") != "") {
		return errors.New("invalid recovery run nonce")
	}
	if r.TokenBudget != nil && *r.TokenBudget <= 0 {
		return errors.New("goal token budget must be positive")
	}
	if r.Action == "budget" && r.TokenBudget == nil {
		return errors.New("goal budget required")
	}
	return nil
}
