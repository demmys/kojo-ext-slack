package slackbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/slack-go/slack"
)

type questionTestManager struct {
	mockMgr
	calls    int
	received agent.QuestionAnswer
}

func (m *questionTestManager) AnswerOneShotQuestion(_ context.Context, _, _ string, a agent.QuestionAnswer) error {
	m.calls++
	m.received = a
	return nil
}
func testSlackQuestion() *slackQuestion {
	return &slackQuestion{token: "opaque", requestID: "r", channel: "C", thread: "T", messageTS: "M", session: "s", user: "U", ctx: context.Background(), questions: []agent.UserQuestion{
		{ID: "color", Question: "Color?", Options: []agent.UserQuestionOption{{Label: "Blue"}, {Label: "Green"}}},
		{Question: "Features?", MultiSelect: true, Options: []agent.UserQuestionOption{{Label: "Fast"}, {Label: "Small"}}},
	}}
}
func questionState() *slack.ViewState {
	return &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
		"choice_0": {"choice": {SelectedOption: slack.OptionBlockObject{Value: "0"}}},
		"text_0":   {"text": {Value: "Custom"}},
		"choice_1": {"choice": {SelectedOptions: []slack.OptionBlockObject{{Value: "1"}}}},
		"text_1":   {"text": {Value: "Extra"}},
	}}
}
func TestQuestionModalAndSubmission(t *testing.T) {
	q := testSlackQuestion()
	modal := questionModal(q)
	if modal.CallbackID != questionCallback || modal.PrivateMetadata != q.token {
		t.Fatal(modal)
	}
	if _, err := json.Marshal(modal); err != nil {
		t.Fatal(err)
	}
	answers, errs := questionSubmission(q, questionState())
	if len(errs) != 0 || answers["color"] != "Custom" {
		t.Fatalf("%v %v", answers, errs)
	}
	vals := answers["Features?"].([]string)
	if len(vals) != 2 || vals[0] != "Small" || vals[1] != "Extra" {
		t.Fatal(vals)
	}
	_, errs = questionSubmission(q, nil)
	if len(errs) != 2 {
		t.Fatal(errs)
	}
	state := questionState()
	state.Values["choice_0"]["choice"] = slack.BlockAction{SelectedOption: slack.OptionBlockObject{Value: "200"}}
	_, errs = questionSubmission(q, state)
	if len(errs) == 0 {
		t.Fatal("invalid option accepted")
	}
}
func TestQuestionInteractionUserFenceAndClaimOnce(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	m := &questionTestManager{}
	b.mgr = m
	q := testSlackQuestion()
	b.questions = map[string]*slackQuestion{q.token: q}
	cb := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission, User: slack.User{ID: "other"}, View: slack.View{CallbackID: questionCallback, PrivateMetadata: q.token, State: questionState()}}
	payload, work := b.prepareQuestionInteraction(cb)
	if payload == nil || work != nil || m.calls != 0 {
		t.Fatal("wrong user admitted")
	}
	cb.User.ID = "U"
	payload, work = b.prepareQuestionInteraction(cb)
	if payload != nil || work == nil || m.calls != 0 {
		t.Fatal("must defer network until ACK")
	}
	if _, duplicate := b.prepareQuestionInteraction(cb); duplicate != nil {
		t.Fatal("duplicate accepted")
	}
	work()
	if m.calls != 1 || m.received.RequestID != "r" || m.received.Answers["color"] != "Custom" {
		t.Fatalf("%+v", m)
	}
	if b.questionOps != 0 {
		t.Fatal("operation leaked")
	}
}
func TestQuestionDeliveryAndExpiry(t *testing.T) {
	var posted string
	updates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "chat.postMessage") {
			posted = r.Form.Get("blocks")
		} else if strings.HasSuffix(r.URL.Path, "chat.update") {
			updates++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C","ts":"M"}`))
	}))
	defer srv.Close()
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	b.api = slack.New("test", slack.OptionAPIURL(srv.URL+"/"))
	m := &questionTestManager{}
	b.mgr = m
	turn := &activeTurn{}
	b.showQuestion(context.Background(), "C", "T", "U", "s", turn, agent.ChatEvent{Type: "user_question", RequestID: "r", Questions: json.RawMessage(`[{"question":"Q?"}]`)})
	if len(b.questions) != 1 || !strings.Contains(posted, questionAction) {
		t.Fatalf("%s %d", posted, len(b.questions))
	}
	b.expireQuestions(turn, "r")
	if len(b.questions) != 0 || updates != 1 {
		t.Fatalf("%d %d", len(b.questions), updates)
	}
	b.showQuestion(context.Background(), "C", "T", "U", "s", turn, agent.ChatEvent{Type: "user_question", RequestID: "secret", Questions: json.RawMessage(`[{"question":"Password?","isSecret":true}]`)})
	if m.calls != 1 || !m.received.Deny || len(b.questions) != 0 {
		t.Fatal("secret question was rendered")
	}
}

type failingQuestionManager struct {
	mockMgr
	answer func() error
}

func (m *failingQuestionManager) AnswerOneShotQuestion(context.Context, string, string, agent.QuestionAnswer) error {
	return m.answer()
}
func TestQuestionFailureRetryAndResolutionRace(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "expired"}[resolve], func(t *testing.T) {
			b := newTestBot(t, agent.SlackBotConfig{})
			defer b.cancel()
			q := testSlackQuestion()
			b.questions = map[string]*slackQuestion{q.token: q}
			b.mgr = &failingQuestionManager{answer: func() error {
				if resolve {
					b.expireQuestions(nil, q.requestID)
				}
				return context.DeadlineExceeded
			}}
			cb := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission, User: slack.User{ID: q.user}, View: slack.View{CallbackID: questionCallback, PrivateMetadata: q.token, State: questionState()}}
			_, work := b.prepareQuestionInteraction(cb)
			if work == nil {
				t.Fatal("missing work")
			}
			work()
			if resolve && len(b.questions) != 0 {
				t.Fatal("expired form resurrected")
			}
			if !resolve && (b.questions[q.token] != q || q.claimed) {
				t.Fatal("pre-delivery failure cannot be retried")
			}
		})
	}
}

func TestQuestionWorkerDoesNotBlockStreamingOnSlowSlack(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	b.api = slack.New("test", slack.OptionAPIURL(srv.URL+"/"))
	b.mgr = &questionTestManager{}
	queue, stop := b.questionWorker(context.Background(), "C", "T", "U", "s", &activeTurn{})
	queue <- agent.ChatEvent{Type: "user_question", RequestID: "r", Questions: json.RawMessage(`[{"question":"Q?"}]`)}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker didn't post")
	}
	select {
	case queue <- agent.ChatEvent{Type: "question_resolved", RequestID: "r"}:
	case <-time.After(time.Second):
		t.Fatal("stream loop blocked on Slack card API")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker cancellation blocked")
	}
}

func TestQuestionNativeChoiceRestriction(t *testing.T) {
	q := testSlackQuestion()
	no := false
	q.questions = q.questions[:1]
	q.questions[0].IsOther = &no
	modal := questionModal(q)
	raw, _ := json.Marshal(modal)
	if strings.Contains(string(raw), `"plain_text_input"`) {
		t.Fatal("native choice-only prompt gained free text")
	}
	state := questionState()
	_, errs := questionSubmission(q, state)
	if len(errs) == 0 {
		t.Fatal("injected free text accepted")
	}
	delete(state.Values, "text_0")
	answers, errs := questionSubmission(q, state)
	if len(errs) > 0 || answers["color"] != "Blue" {
		t.Fatalf("%v %v", answers, errs)
	}
}
