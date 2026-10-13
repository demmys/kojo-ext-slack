# Interactive questions

Claude (`AskUserQuestion`) and Codex (`item/tool/requestUserInput`) can ask
questions while a Kojo turn is running. This is separate from steering: a
question answer resolves the original tool request, never a new chat turn.

This document was written when the Slack bot was part of kojo. The bot now
lives in this package and reaches kojo through its `ext-chat` API; the
behaviour described here is unchanged. Where the text says "the Hub", read
"the device running this extension".

## Slack setup and use

- Enable **Interactivity & Shortcuts → Interactivity** in the Slack app settings.
  Socket Mode carries the interactions; a public Request URL is not needed.
- Update every kojo device involved (the one running this extension and the
  one running the agent) to use interactive questions remotely.
  Older peers ignore the opt-in header and retain ordinary, non-interactive chat.
- When a question appears in the original Slack thread, choose **回答する**.
  The modal offers single/multiple selection and, when the native question
  allows it, free text. Free text overrides
  a single selection, or is appended to multiple selections.
- Only the Slack user who initiated that turn can answer its form. This identity
  is retained for the turn's post-handoff continuation. Other users' ordinary
  thread replies are not interpreted as question answers.
- Closing the modal does not deny the question. Reopen it from the card, or use
  `!stop` to cancel the running turn.
- Answered, expired and stopped questions lose their buttons. A daemon restart
  invalidates old cards even if Slack still displays a button.
- On a transport error, Kojo does not automatically replay the answer. If the
  turn is still active, the user can explicitly retry the same question ID.
  The holder accepts each request at most once; late answers cannot target a
  new turn. A broken CLI answer pipe terminates that CLI rather than leaving a
  consumed question blocked indefinitely. If the original answer was accepted but its acknowledgement was
  lost, a retry is reported as already answered/expired.
- Secret-input prompts are not supported. Do not enter passwords or tokens in
  these forms: ordinary answers are recorded in conversation history.

## Implementation notes

On the kojo side, `OneShotOpts.InteractiveQuestions` is a per-response-surface
capability. It is only enabled for callers that can display and answer
questions; this extension asks for it on every turn it starts. Other one-shot
callers keep their previous non-interactive behavior.

The shared `UserQuestion` contract has an optional `id`: Codex answers use that
ID, while Claude answers retain the original question-text key. Codex JSON-RPC
IDs remain opaque (both strings and numbers are echoed exactly); the UI sees a
fresh prompt UUID, preventing collisions after a CLI restart. Codex server-side
resolution notifications invalidate prompts. RPC questions wait for their answer,
including Default-mode requests carrying `isBlocking=false`: on user-initiated
turns Kojo does not auto-answer those UI-optional requests. Message-delivered async questions remain
answerable while work continues. Kojo only adds a fallback timer for unwatched,
automated RPC questions.

Answers go to kojo through the `ext-chat` steer endpoint, which between devices
shares the existing holder-fenced, no-automatic-replay input transport
(`external-chat/steer`), with an exclusive `question` envelope instead
of `content`. The receiver verifies the agent, session, originating Hub and
pending request before invoking the captured backend answer function. Answers
are not downgraded to steering or FIFO messages on errors. Question lifecycle
events use reliable delivery rather than lossy text-delta forwarding.

Slack acknowledges interactions before opening a modal or contacting a peer.
Card posting/expiry runs on a bounded, ordered per-turn worker, so slow Slack
card APIs cannot starve the streaming heartbeat. Queue saturation stops the
turn explicitly rather than silently dropping a question.
Network work is bounded independently from the Socket Mode event loop. Accepted
answers also join the turn's ordered history so a handoff can retain the Q&A.

References:
- [Slack Socket Mode interactions](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- [Codex App Server](https://learn.chatgpt.com/docs/app-server)

### Codex の非同期質問

`request_user_input_async` は回答待ち RPC ではなく、
`agentMessage` の `delivery: "async"` / `questions` として届く。
これも同じ回答フォームに変換する。選択肢がない質問・自由記述にも対応する。
回答は質問本文と対応づけたユーザー入力として `turn/steer` で届け、
元のツール呼び出しへの JSON-RPC 応答は送らない。

質問中も処理は続く。goal の自動継続ターンを跨いでも同じ実行内なら回答できる。
通常処理の終了、goal の終了、停止、再起動でカードは失効する。
失効後は通常のスレッド返信で回答する（カードの回答から勝手に実行を再開しない）。
回答の到達が不明な場合も自動再送しない。

### Codex の同期質問と Default mode

回答経路のある呼び出しでは、Kojo が app-server 起動時に
`features.default_mode_request_user_input=true` を指定する。
この設定がないと同期 `request_user_input` は Default mode で拒否され、
質問カード用の RPC に到達しない。通常の作業を続けられるよう Plan mode には切り替えない。
同期質問は回答フォームからの入力を元の RPC に返すまで待機する。
通常のスレッド追記は同期質問への回答ではないため、カードの「回答する」を使う。
非対話呼び出しにはこの設定を追加しない。
