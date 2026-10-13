package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
	"github.com/loppo-llc/kojo-ext-slack/internal/chathistory"
	"github.com/slack-go/slack"
)

const questionAction = "kojo_question_answer"
const questionCallback = "kojo_question_submit"

type oneShotQuestionAnswerer interface {
	AnswerOneShotQuestion(context.Context, string, string, agent.QuestionAnswer) error
}

type slackQuestion struct {
	claimed bool // guarded by questionsMu; kept registered until resolution

	token, requestID, channel, thread, messageTS, session, user string
	questions                                                   []agent.UserQuestion
	turn                                                        *activeTurn
	ctx                                                         context.Context
}

func questionPlain(text string, limit int) *slack.TextBlockObject {
	r := []rune(text)
	if len(r) > limit {
		text = string(r[:limit-1]) + "…"
	}
	return slack.NewTextBlockObject(slack.PlainTextType, text, false, false)
}

func (b *Bot) showQuestion(ctx context.Context, channel, thread, user, session string, turn *activeTurn, ev agent.ChatEvent) {
	answerer, ok := b.mgr.(oneShotQuestionAnswerer)
	if !ok {
		return
	} // ChatOneShot only opts in when this capability exists.
	var questions []agent.UserQuestion
	err := json.Unmarshal(ev.Questions, &questions)
	if err == nil && (len(questions) == 0 || len(questions) > 10) {
		err = fmt.Errorf("unsupported question count")
	}
	seen := make(map[string]bool)
	for _, q := range questions {
		if q.Question == "" || len([]rune(q.Question)) > 3000 || q.IsSecret || seen[q.AnswerKey()] || len(q.Options) > 100 {
			err = fmt.Errorf("invalid or secret question")
		}
		seen[q.AnswerKey()] = true
		detailSize := 0
		for _, opt := range q.Options {
			if strings.TrimSpace(opt.Label) == "" {
				err = fmt.Errorf("empty option")
			}
			detailSize += len([]rune(opt.Label)) + len([]rune(opt.Description)) + 16
		}
		if detailSize > 3000 {
			err = fmt.Errorf("options exceed Slack form limits")
		}
	}
	if user == "" {
		err = fmt.Errorf("no initiating user")
	}
	if err != nil {
		_ = answerer.AnswerOneShotQuestion(ctx, b.agentID, session, agent.QuestionAnswer{RequestID: ev.RequestID, Deny: true, DenyMessage: "This question cannot be displayed safely in Slack. Ask a non-secret question in normal chat."})
		b.postMessage(ctx, channel, thread, "この質問はSlackの回答フォームでは扱えません。秘密情報は通常のチャットにも入力しないでください。")
		return
	}
	q := &slackQuestion{token: uuid.NewString(), requestID: ev.RequestID, channel: channel, thread: thread, session: session, user: user, questions: questions, turn: turn, ctx: ctx}
	heading := "回答待ち — この処理を依頼したユーザーが回答できます。"
	if ev.QuestionBlocking != nil && !*ev.QuestionBlocking {
		heading = "質問（処理は続行中）— この処理を依頼したユーザーが回答できます。"
	}
	blocks := []slack.Block{slack.NewSectionBlock(questionPlain(heading, 3000), nil, nil)}
	for _, item := range questions {
		blocks = append(blocks, slack.NewSectionBlock(questionPlain(item.Question, 3000), nil, nil))
	}
	blocks = append(blocks, slack.NewActionBlock("kojo_question", slack.NewButtonBlockElement(questionAction, q.token, questionPlain("回答する", 75))))
	b.questionsMu.Lock()
	if b.questions == nil {
		b.questions = make(map[string]*slackQuestion)
	}
	b.questions[q.token] = q
	b.questionsMu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	postOpts := []slack.MsgOption{slack.MsgOptionText("エージェントからの質問（回答待ち）", false), slack.MsgOptionBlocks(blocks...)}
	if thread != "" {
		postOpts = append(postOpts, slack.MsgOptionTS(thread))
	}
	_, ts, err := b.api.PostMessageContext(callCtx, channel, postOpts...)
	b.questionsMu.Lock()
	q.messageTS = ts
	if err != nil {
		delete(b.questions, q.token)
	}
	b.questionsMu.Unlock()
	if err != nil {
		b.logger.Warn("slack question delivery failed", "err", err)
		_ = answerer.AnswerOneShotQuestion(ctx, b.agentID, session, agent.QuestionAnswer{RequestID: ev.RequestID, Deny: true, DenyMessage: "Slack could not deliver the question form. Ask in normal chat instead."})
		b.postMessage(ctx, channel, thread, "質問フォームを送れませんでした。Slackアプリの Interactivity が有効か確認してください。")
	}
}

func questionModal(q *slackQuestion) slack.ModalViewRequest {
	blocks := []slack.Block{}
	for i, item := range q.questions {
		blocks = append(blocks, slack.NewSectionBlock(questionPlain(item.Question, 3000), nil, nil))
		// Full labels/descriptions remain visible even when an option's short
		// selector label must be truncated to Slack's 75-character limit.
		options := []*slack.OptionBlockObject{}
		if len(item.Options) <= 100 {
			for j, opt := range item.Options {
				options = append(options, slack.NewOptionBlockObject(strconv.Itoa(j), questionPlain(opt.Label, 75), nil))
			}
		}
		if len(options) > 0 {
			var element slack.BlockElement
			if item.MultiSelect {
				element = slack.NewOptionsMultiSelectBlockElement(slack.MultiOptTypeStatic, questionPlain("選択肢", 150), "choice", options...)
			} else {
				element = slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, questionPlain("選択肢", 150), "choice", options...)
			}
			input := slack.NewInputBlock(fmt.Sprintf("choice_%d", i), questionPlain("選択肢（任意）", 2000), nil, element)
			input.Optional = item.AllowsFreeText()
			blocks = append(blocks, input)
		}
		// Descriptions in a section avoid the option-description API length limit.
		var detail strings.Builder
		for j, opt := range item.Options {
			fmt.Fprintf(&detail, "%d. %s — %s\n", j+1, opt.Label, opt.Description)
		}
		if detail.Len() > 0 {
			blocks = append(blocks, slack.NewSectionBlock(questionPlain(detail.String(), 3000), nil, nil))
		}
		if !item.AllowsFreeText() {
			continue
		}
		text := slack.NewPlainTextInputBlockElement(questionPlain("自由記述（選択肢の代わりにも使えます）", 150), "text")
		text.Multiline = true
		text.MaxLength = 3000
		input := slack.NewInputBlock(fmt.Sprintf("text_%d", i), questionPlain("自由記述（任意）", 2000), nil, text)
		input.Optional = true
		blocks = append(blocks, input)
	}
	return slack.ModalViewRequest{Type: slack.VTModal, Title: questionPlain("エージェントへの回答", 24), Submit: questionPlain("送信", 24), Close: questionPlain("閉じる", 24), CallbackID: questionCallback, PrivateMetadata: q.token, Blocks: slack.Blocks{BlockSet: blocks}}
}

func questionSubmission(q *slackQuestion, state *slack.ViewState) (map[string]any, map[string]string) {
	answers := make(map[string]any)
	errs := make(map[string]string)
	if state == nil {
		state = &slack.ViewState{}
	}
	for i, item := range q.questions {
		free := strings.TrimSpace(state.Values[fmt.Sprintf("text_%d", i)]["text"].Value)
		errorBlock := fmt.Sprintf("text_%d", i)
		if !item.AllowsFreeText() {
			errorBlock = fmt.Sprintf("choice_%d", i)
			if free != "" {
				errs[errorBlock] = "自由記述は許可されていません。"
			}
			free = ""
		}
		choice := state.Values[fmt.Sprintf("choice_%d", i)]["choice"]
		selected := choice.SelectedOptions
		if !item.MultiSelect && choice.SelectedOption.Value != "" {
			selected = []slack.OptionBlockObject{choice.SelectedOption}
		}
		values := []string{}
		for _, opt := range selected {
			j, err := strconv.Atoi(opt.Value)
			if err != nil || j < 0 || j >= len(item.Options) {
				errs[errorBlock] = "選択肢が無効です。"
				continue
			}
			values = append(values, item.Options[j].Label)
		}
		if free != "" {
			if !item.MultiSelect {
				values = nil
			}
			values = append(values, free)
		}
		if len(values) == 0 {
			errs[errorBlock] = "選択肢を選ぶか、自由記述を入力してください。"
		}
		if item.MultiSelect {
			answers[item.AnswerKey()] = values
		} else if len(values) > 0 {
			answers[item.AnswerKey()] = values[0]
		}
	}
	if len(errs) == 0 {
		if err := agent.ValidateQuestionAnswers(q.questions, answers); err != nil {
			key := "text_0"
			if len(q.questions) > 0 && !q.questions[0].AllowsFreeText() {
				key = "choice_0"
			}
			errs[key] = "回答が長すぎるか無効です。短くして再送信してください。"
		}
	}
	return answers, errs
}

// prepareQuestionInteraction performs only local work before Socket Mode ACK.
// All network calls (including peer answers) run afterwards, outside the event
// loop. The opaque token is bound to the user who initiated this exact turn.
func (b *Bot) prepareQuestionInteraction(cb slack.InteractionCallback) (any, func()) {
	token := ""
	if cb.Type == slack.InteractionTypeBlockActions {
		for _, a := range cb.ActionCallback.BlockActions {
			if a.ActionID == questionAction {
				token = a.Value
				break
			}
		}
	} else if cb.Type == slack.InteractionTypeViewSubmission && cb.View.CallbackID == questionCallback {
		token = cb.View.PrivateMetadata
	} else {
		return nil, nil
	}
	b.questionsMu.Lock()
	q := b.questions[token]
	valid := q != nil && !q.claimed && q.user == cb.User.ID && q.ctx.Err() == nil
	if valid && cb.Type == slack.InteractionTypeBlockActions {
		valid = q.channel == cb.Channel.ID && (q.messageTS == "" || q.messageTS == cb.Message.Timestamp)
		if valid && q.messageTS == "" {
			q.messageTS = cb.Message.Timestamp
		}
	}
	if !valid {
		b.questionsMu.Unlock()
		if cb.Type == slack.InteractionTypeViewSubmission {
			return slack.NewErrorsViewSubmissionResponse(map[string]string{questionErrorBlock(cb.View): "この質問は期限切れ、回答済み、または別のユーザー宛てです。閉じてください。"}), nil
		}
		return nil, b.questionNoticeWork(cb.Channel.ID, cb.User.ID, "この質問は期限切れ、回答済み、または別のユーザー宛てです。")
	}
	if b.questionOps >= 8 {
		b.questionsMu.Unlock()
		if cb.Type == slack.InteractionTypeViewSubmission {
			return slack.NewErrorsViewSubmissionResponse(map[string]string{questionErrorBlock(cb.View): "混み合っています。少し待って再送信してください。"}), nil
		}
		return nil, nil
	}
	var answers map[string]any
	var admission *activeSteerReservation
	if cb.Type == slack.InteractionTypeViewSubmission {
		var errs map[string]string
		answers, errs = questionSubmission(q, cb.View.State)
		if len(errs) > 0 {
			b.questionsMu.Unlock()
			return slack.NewErrorsViewSubmissionResponse(errs), nil
		}
		// Claim once, before acknowledging. Never retry an uncertain delivery as
		// either another answer or a normal user message.
		q.claimed = true
		if q.turn != nil {
			admission = q.turn.reserveSteer(false)
		}
	}
	b.questionOps++
	b.questionsMu.Unlock()
	return nil, func() {
		defer func() { b.questionsMu.Lock(); b.questionOps--; b.questionsMu.Unlock() }()
		if admission != nil {
			admission.Wait()
			defer admission.Release()
		}
		ctx, cancel := context.WithTimeout(q.ctx, 35*time.Second)
		defer cancel()
		if cb.Type == slack.InteractionTypeBlockActions {
			if _, err := b.api.OpenViewContext(ctx, cb.TriggerID, questionModal(q)); err != nil {
				b.logger.Warn("open question modal failed", "err", err)
				b.questionEphemeral(q.channel, q.user, "回答フォームを開けませんでした。もう一度ボタンを押してください。")
			}
			return
		}
		responder, ok := b.mgr.(oneShotQuestionAnswerer)
		if !ok {
			b.updateQuestion(q, "質問への回答はこの接続では使えません。")
			return
		}
		err := responder.AnswerOneShotQuestion(ctx, b.agentID, q.session, agent.QuestionAnswer{RequestID: q.requestID, Answers: answers})
		if err != nil {
			b.logger.Warn("slack question answer failed", "err", err)
			b.questionsMu.Lock()
			retry := b.questions[q.token] == q && q.ctx.Err() == nil && !errors.Is(err, agent.ErrAgentNotBusy) && !errors.Is(err, agent.ErrQuestionNotFound)
			if retry {
				q.claimed = false
			} else {
				delete(b.questions, q.token)
			}
			b.questionsMu.Unlock()
			if retry {
				b.updateQuestionRetry(q, "回答の受け付けを確認できませんでした。自動再送はしていません。接続を確認後、同じ質問に回答し直せます。")
			} else {
				b.updateQuestion(q, "この質問は期限切れ、回答済み、または処理が停止しています。")
			}
			return
		}
		b.questionsMu.Lock()
		delete(b.questions, q.token)
		b.questionsMu.Unlock()
		var text strings.Builder
		text.WriteString("回答済み\n")
		for _, item := range q.questions {
			fmt.Fprintf(&text, "%s → %v\n", item.Question, answers[item.AnswerKey()])
		}
		if q.turn != nil {
			q.turn.steerHistory = append(q.turn.steerHistory, chathistory.HistoryMessage{
				Platform: platformSlack, ChannelID: q.channel, ThreadID: q.thread, MessageID: "question:" + q.token,
				UserID: q.user, UserName: q.user, Text: text.String(), Timestamp: time.Now().Format(time.RFC3339),
			})
		}
		b.updateQuestion(q, text.String())
	}
}
func (b *Bot) questionEphemeral(channel, user, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.api.PostEphemeralContext(ctx, channel, user, slack.MsgOptionText(text, false))
}
func (b *Bot) updateQuestion(q *slackQuestion, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b.updateQuestionCard(ctx, q, text, false)
}
func (b *Bot) updateQuestionCard(ctx context.Context, q *slackQuestion, text string, retry bool) {
	b.questionsMu.Lock()
	ts := q.messageTS
	b.questionsMu.Unlock()
	if ts == "" {
		return
	}
	blocks := []slack.Block{slack.NewSectionBlock(questionPlain(text, 3000), nil, nil)}
	if retry {
		blocks = append(blocks, slack.NewActionBlock("kojo_question", slack.NewButtonBlockElement(questionAction, q.token, questionPlain("回答し直す", 75))))
	}
	_, _, _, err := b.api.UpdateMessageContext(ctx, q.channel, ts, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...))
	if err != nil {
		b.logger.Warn("update slack question failed", "err", err)
	}
}
func (b *Bot) expireQuestions(turn *activeTurn, requestID string) {
	b.questionsMu.Lock()
	var expired []*slackQuestion
	for token, q := range b.questions {
		if q.turn == turn && (requestID == "" || q.requestID == requestID) {
			delete(b.questions, token)
			if !q.claimed {
				expired = append(expired, q)
			}
		}
	}
	b.questionsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, q := range expired {
		if ctx.Err() != nil {
			break
		}
		b.updateQuestionCard(ctx, q, "この質問は回答済み、期限切れ、または処理終了により無効になりました。", false)
	}
}

// A user may explicitly retry a failed transport, using the same request ID.
// The holder claims that ID once, so this cannot answer a replacement prompt.
func (b *Bot) updateQuestionRetry(q *slackQuestion, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b.updateQuestionCard(ctx, q, text, true)
}

func (b *Bot) questionNoticeWork(channel, user, text string) func() {
	b.questionsMu.Lock()
	defer b.questionsMu.Unlock()
	if b.questionOps >= 8 {
		return nil
	}
	b.questionOps++
	return func() {
		defer func() { b.questionsMu.Lock(); b.questionOps--; b.questionsMu.Unlock() }()
		b.questionEphemeral(channel, user, text)
	}
}

// A per-turn ordered worker keeps Slack card API latency out of the streaming
// heartbeat loop. Queue saturation cancels the turn instead of losing a prompt.
func (b *Bot) questionWorker(ctx context.Context, channel, thread, user, session string, turn *activeTurn) (chan<- agent.ChatEvent, func()) {
	workerCtx, cancel := context.WithCancel(ctx)
	queue := make(chan agent.ChatEvent, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-workerCtx.Done():
				return
			case ev := <-queue:
				if workerCtx.Err() != nil {
					return
				}
				if ev.Type == "user_question" {
					b.showQuestion(workerCtx, channel, thread, user, session, turn, ev)
				} else {
					b.expireQuestions(turn, ev.RequestID)
				}
			}
		}
	}()
	return queue, func() { cancel(); <-done; b.expireQuestions(turn, "") }
}

func questionErrorBlock(view slack.View) string {
	if view.State != nil {
		if _, ok := view.State.Values["text_0"]; ok {
			return "text_0"
		}
	}
	return "choice_0"
}
