# kojo-ext-slack

[日本語](README.ja.md)

An extension package that connects a [kojo](https://github.com/loppo-llc/kojo) agent to Slack. It is the Slack integration that used to be built into kojo, moved into a package; it behaves the same way.

- Send the bot a **DM**, or **@mention** it in a channel, and the agent answers
- In a **thread** the bot has answered in, the conversation continues without mentions
- The answer is **streamed** into Slack while the agent works, with the tools it is running shown as status
- A message written while the agent is working is handed to the **running turn** as a follow-up
- **Files** attached to a message are handed to the agent
- `!stop` stops the running turn; `!goal` drives a long-running goal
- When the agent asks a **question**, it appears as a card with a form
- **Background tasks** started in a thread report back into that thread when they finish
- The agent can also **act on its own** (post, reply, react, read history, upload files)
- Each Slack thread is its own session, separate from the chat in the kojo UI

One Slack app per agent. To connect three agents, create three apps.

## Requirements

- A kojo build **newer than v0.135.0** (needs the `chat:session` scope, extension secret settings, and the extension chat API for the `slack` package)
- A Slack workspace where you may create and install an app

The bot connects out to Slack over Socket Mode. kojo does not have to be reachable from the internet and no Request URL is involved.

## 1. Slack setup

### 1-1. Create an app

1. Open <https://api.slack.com/apps> and click **Create New App → From scratch**
2. Enter a name (it becomes the bot's display name), pick the workspace, and click **Create App**

### 1-2. Enable Socket Mode and issue the app-level token

1. Open **Socket Mode** in the left menu and turn on **Enable Socket Mode**
2. You are asked to generate an app-level token. Give it any name, make sure it has the **`connections:write`** scope, and click **Generate**
3. **Copy** the token. It starts with `xapp-`

The token can be viewed again later under **Basic Information → App-Level Tokens**.

### 1-3. Bot token scopes

Open **OAuth & Permissions** and add these under **Scopes → Bot Token Scopes**.

Needed for the bot to answer:

| Scope | Used for |
| --- | --- |
| `chat:write` | posting, streaming, updating and deleting the bot's messages |
| `app_mentions:read` | receiving @mentions |
| `im:history` | receiving DMs and reading their history |
| `channels:history` | receiving thread follow-ups in public channels and reading thread history |
| `groups:history` | the same in private channels |
| `mpim:history` | the same in group DMs |
| `users:read` | showing the agent who wrote each message by name |
| `files:read` | downloading files attached to a message |
| `channels:read` | resolving public channel names, and `slack_list_channels` |
| `groups:read` | the same for private channels |

Optional:

| Scope | Used for |
| --- | --- |
| `assistant:write` | the "is thinking…" status under the thread while the agent works. Also needs **Agents & AI Apps** turned on for the app. Without it the bot works the same, minus the status line |

Needed for the tools the agent uses on its own (see [Tools the agent gets](#tools-the-agent-gets)); leave out the ones you do not want it to have:

| Scope | Tools |
| --- | --- |
| `reactions:write` | `slack_add_reaction`, `slack_remove_reaction` |
| `emoji:read` | `slack_list_emoji` |
| `files:write` | `slack_upload_file` |
| `chat:write.public` | posting to public channels the bot is not a member of |

`chat:write`, `users:read` and the `*:history` scopes above are also what `slack_post_message`, `slack_reply_to_thread`, `slack_list_users`, `slack_get_channel_history` and `slack_get_thread_replies` use.

If all you want is DMs and @mentions, `chat:write`, `app_mentions:read` and `im:history` are enough to start with: the bot answers, but it shows user IDs instead of names, cannot receive files, and does not follow up in channel threads without a mention.

### 1-4. Event subscriptions

Open **Event Subscriptions**, turn on **Enable Events**, and add these under **Subscribe to bot events**:

| Event | Used for |
| --- | --- |
| `app_mention` | @mentions in channels |
| `message.im` | DMs |
| `message.channels` | thread follow-ups without a mention, in public channels |
| `message.groups` | the same in private channels |
| `message.mpim` | the same in group DMs |

Click **Save Changes**. With Socket Mode on there is no Request URL to fill in.

### 1-5. Interactivity

Open **Interactivity & Shortcuts** and turn on **Interactivity**. This is what delivers the button and the form of a question the agent asks. Socket Mode carries it; no Request URL is needed.

### 1-6. Allow DMs

Open **App Home**, and under **Show Tabs** turn on **Messages Tab** and tick **Allow users to send Slash commands and messages from the messages tab**. Without this, users cannot write to the bot in a DM.

### 1-7. Install the app and copy the bot token

1. Open **OAuth & Permissions** (or **Install App**) and click **Install to Workspace**, then **Allow**
2. **Copy** the **Bot User OAuth Token**. It starts with `xoxb-`

Whenever you change scopes later, Slack asks you to **reinstall** the app. The bot token may change on reinstall, so copy it again from **OAuth & Permissions** and paste it into the extension settings.

Both tokens are passwords. Do not paste them into chats or repositories. If one leaks, revoke it on the same page it was issued on.

### 1-8. Invite the bot to channels

The bot only sees channels it is a member of. In each channel where it should answer, run `/invite @botname`.

## 2. kojo setup

1. Open **Settings → Extensions** in kojo
2. Enter `https://github.com/loppo-llc/kojo-ext-slack` as the **repository URL** and click the button that inspects it
3. Review the requested scope (`chat:session` — talk to the agent in a dedicated session) and approve the install
4. Open **Slack** in the installed list and enable it for the agent you want to connect
5. Paste the token from 1-2 into **App-level token** and the token from 1-7 into **Bot token** in that agent's settings, and save

Saving starts the service for that agent. Send the bot a DM and it answers.

The tokens are stored encrypted in kojo's credential store and never appear again in the UI or the API. To replace one, enter a new value and save.

### Settings

| Setting | Default | Description |
| --- | --- | --- |
| App-level token | — | Required. Starts with `xapp-` |
| Bot token | — | Required. Starts with `xoxb-` |
| Always reply in a thread | on | Answer a top-level message in a thread on that message; each thread is a separate conversation. When off, the bot answers in the channel or DM itself and the whole channel is one conversation |
| Answer direct messages | on | Answer every message in a DM with the bot |
| Answer @mentions in channels | on | Answer when @mentioned in a channel the bot is a member of |
| Keep answering in threads | on | In a thread where the bot gave the last answer, answer follow-ups without a mention. A message that mentions someone else is left alone |

### Mind who can talk to it

Anyone who can write to the bot in Slack **can give instructions to that agent** — including whatever shell and file access the agent has. There is no allowlist: everyone in the workspace can DM the bot, and every member of a channel it was invited to can mention it. Install the app only in a workspace where that is acceptable, and invite the bot only to channels whose members you trust.

## Usage

- **Talk to it**: write to the bot in a DM, or write `@botname hello` in a channel
- **Follow up**: reply in the thread. In a channel thread no mention is needed as long as the bot's answer is the last thing it knows of there
- **Add to a running turn**: write again in the same thread while the agent is working and the message is handed to the running turn. Backends that cannot take input mid-turn get it as the next turn instead; messages are handled in the order they were written
- **Stop**: write `!stop` (or `!cancel`) in the thread. In a channel, only the person who started the turn can stop it. What the agent had written so far stays. `!stop all` also stops the background tasks the thread started
- **Goals**: `!goal <objective>` starts a long-running goal that the agent keeps working on across turns. `!goal` or `!goal status` shows it, `!goal pause`, `!goal resume` and `!goal clear` control it, and `!goal budget N` / `!goal --tokens N <objective>` set a token limit. Only the person who started a goal can change it. Goals need a backend that supports them
- **Questions**: when the agent asks a question, a card with a **回答する** (answer) button appears in the thread. The button opens a form. Only the person who started the turn can answer. Closing the form does not refuse the question; reopen it from the card, or `!stop` the turn
- **Files**: attach files to a message and the agent gets them
- **Background tasks**: if a turn ends while tasks it started are still running, the bot says so, and posts the result in the same thread when they finish
- **Sometimes it stays silent**: when the agent decides no answer is needed, nothing is posted
- **Errors**: if a turn fails, the reason is posted in the thread, after whatever part of the answer had been produced
- **Conversation scope**: one session per thread (or per channel / DM when "Always reply in a thread" is off). It does not remember other threads, though the agent's own memory files are shared

### Tools the agent gets

An agent with this extension enabled gains these tools from the MCP server `slack`. They work from the kojo UI and from cron too, so "post a summary to #news every morning" is possible.

| Tool | What it does |
| --- | --- |
| `slack_list_channels` | List the channels the bot can see |
| `slack_post_message` | Post a message to a channel |
| `slack_reply_to_thread` | Reply in a thread |
| `slack_add_reaction` | Add an emoji reaction to a message |
| `slack_remove_reaction` | Remove one of the bot's reactions |
| `slack_list_emoji` | List the workspace's custom emoji |
| `slack_get_channel_history` | Read recent messages in a channel |
| `slack_get_thread_replies` | Read a thread |
| `slack_list_users` | List the workspace's members |
| `slack_upload_file` | Upload a file to a channel or thread |

The tools use the bot token, so they can do what the app's scopes allow (1-3) in the channels the bot is a member of.

## Migrating from the built-in Slack integration

kojo used to ship this bot built in, configured under each agent's settings. Versions of kojo that no longer contain it move an agent over when they start: they install this package, enable it for every agent whose Slack bot was enabled, and carry over the two tokens, the four switches and the thread history. If that happened, there is nothing to do; check **Settings → Extensions → Slack** to confirm that the agent is enabled there. If kojo could not finish (no network, for example), the agent's settings page shows a notice with the reason and a retry button, and kojo tries again each time it starts.

To move an agent over by hand:

1. Install the package and enable it for the agent (section 2)
2. Enter the **same** app-level token and bot token the built-in integration used, and set the four switches the way they were. The Slack app itself needs no change: the scopes, events and interactivity setting are the ones the built-in integration needed
3. On a kojo that still contains the built-in bot, make sure it is off for that agent. Two connections on one app would each receive only part of the events

What carries over:

- **Threads keep their sessions.** A thread is still the session `<agent>:slack:<channel>:<thread>`, so a reply in an old thread continues the conversation the agent had there, including a goal in progress
- **Settings** have the same names and meanings: `threadReplies`, `respondDM`, `respondMention`, `respondThread`
- **Tools** have the same names and arguments. The MCP server is still called `slack`
- **Commands and messages** (`!stop`, `!stop all`, `!goal`, question cards, notices) are unchanged

What does not carry over by itself:

- **The record of which threads the bot is following.** It is kept as files, formerly under the agent's data directory in `chat_history/slack/`, now under this extension's data directory in the same layout (`chat_history/slack/<channel>/<thread>.jsonl`). kojo copies them when it migrates an agent. If you moved by hand and did not copy them, the bot does not recognise old threads as its own until it is @mentioned in them once; after that it follows up without a mention again. Nothing the agent remembers is lost either way
- **Turns that were running** when kojo was updated. Send the message again

## Troubleshooting

| Symptom | Check |
| --- | --- |
| The bot does not come online | Were both tokens saved? Are both the extension and the agent enabled? The kojo log says what is wrong: `the app-level token and the bot token are both required`, `appToken must start with xapp-` / `botToken must start with xoxb-` (the two are swapped, or something else was pasted), or `Slack refused the bot token` (reinstall the app and copy the token again) |
| The log shows connection errors after `Slack accepted the bot token` | The app-level token is wrong or lacks `connections:write`, or Socket Mode is off (1-2) |
| No answer to a DM | Is `message.im` subscribed (1-4)? Is the Messages Tab enabled (1-6)? Is "Answer direct messages" on? |
| No answer to a mention | Is `app_mention` subscribed? Was the bot invited to the channel (1-8)? Is "Answer @mentions in channels" on? |
| No answer to thread follow-ups | Are `message.channels` / `message.groups` subscribed, with the matching `*:history` scopes? Is "Keep answering in threads" on? Does the message mention someone else? |
| Messages show user IDs instead of names | Add `users:read` and reinstall |
| Attached files do not reach the agent | Add `files:read` and reinstall |
| The answer appears all at once instead of streaming | Slack refused to stream into that conversation; the bot falls back to posting the finished answer. Nothing is lost |
| The 回答する button does nothing | Turn on Interactivity (1-5) |
| A tool fails with `missing_scope` | Add the scope the tool needs (1-3), reinstall, and paste the bot token again |
| A tool fails with `not_in_channel` | Invite the bot to the channel, or add `chat:write.public` |
| After changing scopes nothing changes | Slack applies new scopes only after the app is reinstalled; paste the bot token again afterwards |

Logs go to kojo's own log, tagged `extension=slack`.

## Further reading

- [docs/slack-errors.md](docs/slack-errors.md) — how a failed turn is reported in Slack
- [docs/slack-questions.md](docs/slack-questions.md) — question cards: setup, rules, implementation notes

## Limitations

- No slash commands. Commands are ordinary messages starting with `!`
- No allowlist of users or channels
- Edits and deletions of a message are not passed on to the agent
- The tools are available on the device where this package is installed. An agent that has moved to another device gets them there only if the package is installed there too

## Development

```sh
make test    # go vet + go test
make build   # build binaries for every platform under bin/
```

kojo installs an extension package by `git clone` and does not build it, so the binaries are committed to the repository. After changing code, run `make build`, commit `bin/` along with it, and bump `version` in `kojo-package.json`.

The same binary plays two roles: with no arguments it is the service that stays connected to Slack; with `mcp` it is a stdio MCP server.

| Directory | Contents |
| --- | --- |
| `internal/slackbot` | the bot: Socket Mode events, per-thread ordering, streaming, commands, questions |
| `internal/kojoclient` | the client for kojo's `ext-chat` API that the bot runs its turns through |
| `internal/chathistory` | the per-thread history files |
| `internal/agent`, `internal/uploadpath` | the types and paths shared with kojo |

### Testing against a fake Slack

For tests only, the environment variable `EXT_SLACK_API_URL` replaces the address of Slack's Web API (for example `http://127.0.0.1:9000/api/`). Both the service and the MCP server read it once at startup, and the Socket Mode address comes from whatever that server answers to `apps.connections.open`. kojo starts the package with a fixed set of variables, so the way to set it is the `env` map of `contributes.service` and of the MCP server in a test copy of `kojo-package.json`. Leave it unset in normal use: with it set, the tokens are sent to that address instead of Slack.

## License

MIT
