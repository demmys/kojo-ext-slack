package agent

import "testing"

func TestQuestionSingleChoiceAndTransportLimits(t *testing.T) {
	qs := []UserQuestion{{ID: "q", Question: "Q?"}}
	if err := ValidateQuestionAnswers(qs, map[string]any{"q": []string{"a", "b"}}); err == nil {
		t.Fatal("single choice accepted multiple answers")
	}
	if err := ValidateQuestionAnswers(qs, map[string]any{"q": 123}); err == nil {
		t.Fatal("non-string accepted")
	}
}
