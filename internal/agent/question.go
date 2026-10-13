package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// UserQuestion is the shared presentation contract. ID is the Codex answer key;
// Claude uses the question text instead. Secret prompts are deliberately not
// accepted: answers are recorded in conversation history, not a secret store.
type UserQuestion struct {
	ID          string               `json:"id,omitempty"`
	Question    string               `json:"question"`
	Header      string               `json:"header,omitempty"`
	Options     []UserQuestionOption `json:"options,omitempty"`
	MultiSelect bool                 `json:"multiSelect,omitempty"`
	IsOther     *bool                `json:"isOther,omitempty"`
	IsSecret    bool                 `json:"isSecret,omitempty"`
}
type UserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func (q UserQuestion) AllowsFreeText() bool {
	return len(q.Options) == 0 || q.IsOther == nil || *q.IsOther
}

func (q UserQuestion) AnswerKey() string {
	if q.ID != "" {
		return q.ID
	}
	return q.Question
}

func ValidateQuestionAnswers(questions []UserQuestion, answers map[string]any) error {
	// Leave room for the request/session envelope in the peer's 64 KiB body cap.
	encoded, err := json.Marshal(answers)
	if err != nil || len(encoded) > 60<<10 {
		return ErrInvalidQuestionAnswer
	}
	if len(questions) == 0 || len(answers) != len(questions) {
		return fmt.Errorf("%w: answer every question", ErrInvalidQuestionAnswer)
	}
	for _, q := range questions {
		if q.IsSecret {
			return fmt.Errorf("%w: secret input is not supported", ErrInvalidQuestionAnswer)
		}
		v, ok := answers[q.AnswerKey()]
		if !ok {
			return fmt.Errorf("%w: missing answer", ErrInvalidQuestionAnswer)
		}
		vals, err := questionAnswerStrings(v)
		if err != nil || len(vals) == 0 || (!q.MultiSelect && len(vals) > 1) {
			return ErrInvalidQuestionAnswer
		}
		for _, s := range vals {
			if !q.AllowsFreeText() {
				allowed := false
				for _, opt := range q.Options {
					if s == opt.Label {
						allowed = true
						break
					}
				}
				if !allowed {
					return ErrInvalidQuestionAnswer
				}
			}
			if strings.TrimSpace(s) == "" || len(s) > 16384 {
				return ErrInvalidQuestionAnswer
			}
		}
	}
	return nil
}
func questionAnswerStrings(v any) ([]string, error) {
	switch v := v.(type) {
	case string:
		return []string{v}, nil
	case []string:
		return v, nil
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, ErrInvalidQuestionAnswer
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, ErrInvalidQuestionAnswer
	}
}

// QuestionAnswer is the common answer envelope used by external transports.
type QuestionAnswer struct {
	RequestID   string         `json:"requestId"`
	Answers     map[string]any `json:"answers,omitempty"`
	Deny        bool           `json:"deny,omitempty"`
	DenyMessage string         `json:"denyMessage,omitempty"`
}
