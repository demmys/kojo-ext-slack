# kojo-ext-slack

[English](README.md)

[kojo](https://github.com/loppo-llc/kojo) のエージェントを Slack につなぐ拡張パッケージ。kojo 本体に組み込まれていた Slack 連携をパッケージに切り出したもので、動作は同じ。

- bot に **DM** を送るか、チャンネルで **@メンション** すると、エージェントが返信する
- 一度 bot が返信した **スレッド** では、以降メンションなしで会話が続く
- 返信はエージェントの作業中から Slack に **ストリーミング** され、実行中のツールがステータスとして見える
- 作業中に書いたメッセージは、**実行中のターン** に追加の指示として渡る
- メッセージに付いた **ファイル** はエージェントに渡る
- `!stop` で実行中のターンを止められる。`!goal` で長時間の目標を進められる
- エージェントからの **質問** は、フォーム付きのカードとして表示される
- スレッドで始めた **バックグラウンド処理** は、終わると同じスレッドに結果を投稿する
- エージェント側からも **自発的に操作** できる (投稿、返信、リアクション、履歴の読み取り、ファイルのアップロード)
- 会話は Slack のスレッド単位で独立したセッションになり、kojo の画面上のチャットとは混ざらない

1 エージェントにつき 1 つの Slack アプリを割り当てる。エージェントを 3 体つなぎたければアプリも 3 つ作る。

## 必要なもの

- kojo **v0.135.0 より新しいビルド** (`chat:session` 権限、拡張機能の secret 設定、`slack` パッケージ向けの拡張チャット API が必要)
- アプリを作成・インストールできる Slack ワークスペース

bot は Socket Mode で Slack へ自分から接続する。kojo をインターネットに公開する必要はなく、Request URL も使わない。

## 1. Slack 側の準備

### 1-1. アプリを作る

1. <https://api.slack.com/apps> を開き、**Create New App → From scratch** を押す
2. 名前を入れ (これが bot の表示名になる)、ワークスペースを選んで **Create App**

### 1-2. Socket Mode を有効にし、App-Level Token を発行する

1. 左メニューの **Socket Mode** を開き、**Enable Socket Mode** をオンにする
2. App-Level Token の生成を求められる。名前は何でもよい。スコープに **`connections:write`** が入っていることを確かめて **Generate**
3. トークンを **Copy** する。`xapp-` で始まる

このトークンは後から **Basic Information → App-Level Tokens** でも確認できる。

### 1-3. Bot Token Scopes

**OAuth & Permissions** を開き、**Scopes → Bot Token Scopes** に次を追加する。

bot が返信するために必要なもの:

| スコープ | 用途 |
| --- | --- |
| `chat:write` | bot のメッセージの投稿・ストリーミング・更新・削除 |
| `app_mentions:read` | @メンションの受信 |
| `im:history` | DM の受信と履歴の読み取り |
| `channels:history` | パブリックチャンネルでのスレッドの続きの受信と、スレッド履歴の読み取り |
| `groups:history` | プライベートチャンネルでの同じ用途 |
| `mpim:history` | グループ DM での同じ用途 |
| `users:read` | 各メッセージの書き手を名前でエージェントに伝える |
| `files:read` | メッセージに添付されたファイルのダウンロード |

任意:

| スコープ | 用途 |
| --- | --- |
| `assistant:write` | 作業中にスレッドの下に出る「考え中…」のステータス。アプリの **Agents & AI Apps** も有効にする必要がある。なくても動作は変わらず、ステータス行が出ないだけ |

エージェントが自発的に使うツール ([エージェントが使えるツール](#エージェントが使えるツール)) に必要なもの。持たせたくないものは外してよい:

| スコープ | ツール |
| --- | --- |
| `channels:read`, `groups:read` | `slack_list_channels`、および各ツールでチャンネルを名前で指定すること |
| `reactions:write` | `slack_add_reaction`, `slack_remove_reaction` |
| `emoji:read` | `slack_list_emoji` |
| `files:write` | `slack_upload_file` |
| `chat:write.public` | bot が参加していないパブリックチャンネルへの投稿 |

`slack_post_message`, `slack_reply_to_thread`, `slack_list_users`, `slack_get_channel_history`, `slack_get_thread_replies` は、上に挙げた `chat:write`, `users:read`, `*:history` を使う。

DM と @メンションだけでよければ、まずは `chat:write`, `app_mentions:read`, `im:history` の 3 つで足りる。bot は返信するが、名前の代わりにユーザー ID が見え、ファイルは受け取れず、チャンネルのスレッドではメンションなしの続きに反応しない。

### 1-4. Event Subscriptions

**Event Subscriptions** を開き、**Enable Events** をオンにして、**Subscribe to bot events** に次を追加する:

| イベント | 用途 |
| --- | --- |
| `app_mention` | チャンネルでの @メンション |
| `message.im` | DM |
| `message.channels` | パブリックチャンネルでの、メンションなしのスレッドの続き |
| `message.groups` | プライベートチャンネルでの同じ用途 |
| `message.mpim` | グループ DM での同じ用途 |

**Save Changes** を押す。Socket Mode がオンなら Request URL の入力欄はない。

### 1-5. Interactivity

**Interactivity & Shortcuts** を開き、**Interactivity** をオンにする。エージェントからの質問のボタンとフォームは、これがないと届かない。Socket Mode 経由で届くので Request URL は不要。

### 1-6. DM を許可する

**App Home** を開き、**Show Tabs** の **Messages Tab** をオンにして、**Allow users to send Slash commands and messages from the messages tab** にチェックを入れる。これがないと、ユーザーは bot に DM を書けない。

### 1-7. アプリをインストールして Bot Token を控える

1. **OAuth & Permissions** (または **Install App**) を開き、**Install to Workspace** → **Allow**
2. **Bot User OAuth Token** を **Copy** する。`xoxb-` で始まる

後からスコープを変えると、Slack はアプリの **再インストール** を求める。Bot Token は変わらない。

2 つのトークンはどちらもパスワードそのもの。チャットやリポジトリに貼らない。漏れたら、発行したのと同じページで失効させる。

### 1-8. bot をチャンネルに招待する

bot に見えるのは、自分が参加しているチャンネルだけ。返信させたいチャンネルごとに `/invite @bot名` を実行する。

## 2. kojo 側の設定

1. kojo の **設定 → 拡張機能** を開く
2. **リポジトリ URL** に `https://github.com/loppo-llc/kojo-ext-slack` を入れ、内容を確認するボタンを押す
3. 要求される権限 (`chat:session` — 専用セッションでエージェントと会話する) を確認し、インストールを承認する
4. インストール済み一覧の **Slack** を開き、つなぎたいエージェントで有効にする
5. そのエージェントの設定で、1-2 のトークンを **App-level token** に、1-7 のトークンを **Bot token** に貼って保存する

保存するとそのエージェント用のサービスが起動する。bot に DM を送れば返信が来る。

トークンは kojo の認証情報ストアに暗号化して保存され、以後 UI にも API にも現れない。差し替えるときは新しい値を入れて保存する。

### 設定項目

| 項目 | 既定 | 説明 |
| --- | --- | --- |
| App-level token | — | 必須。`xapp-` で始まる |
| Bot token | — | 必須。`xoxb-` で始まる |
| Always reply in a thread | オン | トップレベルのメッセージには、そのメッセージのスレッドで返信する。スレッドごとに別の会話になる。オフにすると、チャンネルや DM にそのまま返信し、チャンネル全体が 1 つの会話になる |
| Answer direct messages | オン | bot との DM のメッセージすべてに返信する |
| Answer @mentions in channels | オン | bot が参加しているチャンネルで @メンションされたら返信する |
| Keep answering in threads | オン | 最後に bot が返信したスレッドでは、メンションなしの続きにも返信する。ほかの人をメンションしたメッセージには反応しない |

### 誰が話しかけられるかに注意

Slack で bot に書ける人は誰でも、**そのエージェントに指示を出せる**。エージェントが持つシェルやファイルへのアクセスも含めて、である。許可リストはない。ワークスペースの全員が bot に DM でき、bot を招待したチャンネルの全メンバーがメンションできる。それで構わないワークスペースにだけアプリをインストールし、メンバーを信頼できるチャンネルにだけ bot を招待する。

## 使い方

- **話しかける**: bot に DM を書くか、チャンネルで `@bot名 こんにちは` と書く
- **続ける**: スレッドに返信する。チャンネルのスレッドでも、bot の返信がそのスレッドで bot の知る最後の発言である限り、メンションは要らない
- **実行中のターンに追加する**: エージェントの作業中に同じスレッドへもう一度書くと、実行中のターンに渡る。ターンの途中で入力を受け取れないバックエンドでは、次のターンとして渡る。メッセージは書かれた順に処理される
- **止める**: スレッドに `!stop` (または `!cancel`) と書く。チャンネルでは、そのターンを始めた本人だけが止められる。そこまでにエージェントが書いた分は残る。`!stop all` は、そのスレッドが始めたバックグラウンド処理も止める
- **目標 (goal)**: `!goal <目標>` で、エージェントがターンをまたいで取り組み続ける長時間の目標を始める。`!goal` または `!goal status` で確認、`!goal pause` / `!goal resume` / `!goal clear` で操作、`!goal budget N` / `!goal --tokens N <目標>` でトークン上限を設定する。目標を変更できるのは、始めた本人だけ。対応するバックエンドが必要
- **質問**: エージェントが質問すると、**回答する** ボタンの付いたカードがスレッドに出る。ボタンを押すとフォームが開く。回答できるのは、そのターンを始めた本人だけ。フォームを閉じても質問を拒否したことにはならない。カードから開き直すか、`!stop` でターンを止める
- **ファイル**: メッセージにファイルを添付すると、エージェントに渡る
- **バックグラウンド処理**: ターンが始めた処理が動いたままターンが終わると、bot はそれを知らせ、終わったときに同じスレッドへ結果を投稿する
- **黙っていることもある**: 返信は不要だとエージェントが判断したときは、何も投稿されない
- **エラー**: ターンが失敗すると、そこまでに出来ていた返信の後に、理由がスレッドに投稿される
- **会話の範囲**: スレッドごとに 1 セッション ("Always reply in a thread" がオフならチャンネル / DM ごと)。ほかのスレッドの会話は覚えていないが、エージェント自身のメモリファイルは共有される

### エージェントが使えるツール

この拡張を有効にしたエージェントは、MCP サーバー `slack` から次のツールを得る。kojo の画面からのチャットや cron からも使えるので、「毎朝 #news に要約を投稿する」といったこともできる。

| ツール | 内容 |
| --- | --- |
| `slack_list_channels` | bot に見えるチャンネルの一覧 |
| `slack_post_message` | チャンネルにメッセージを投稿する |
| `slack_reply_to_thread` | スレッドに返信する |
| `slack_add_reaction` | メッセージに絵文字リアクションを付ける |
| `slack_remove_reaction` | bot が付けたリアクションを外す |
| `slack_list_emoji` | ワークスペースのカスタム絵文字の一覧 |
| `slack_get_channel_history` | チャンネルの最近のメッセージを読む |
| `slack_get_thread_replies` | スレッドを読む |
| `slack_list_users` | ワークスペースのメンバーの一覧 |
| `slack_upload_file` | チャンネルやスレッドにファイルをアップロードする |

ツールは Bot Token を使うので、アプリのスコープ (1-3) が許す範囲のことを、bot が参加しているチャンネルで行える。

## 組み込みの Slack 連携からの移行

kojo は以前、この bot を本体に組み込み、各エージェントの設定画面で設定していた。組み込み版を含まなくなったバージョンの kojo は、起動時にエージェントを移行する。このパッケージをインストールし、Slack bot を有効にしていたエージェントすべてで有効にして、2 つのトークン、4 つのスイッチ、スレッドの履歴を引き継ぐ。そうなっていれば、することは何もない。**設定 → 拡張機能 → Slack** で、そのエージェントが有効になっていることだけ確かめる。移行を終えられなかった場合 (ネットワークにつながらない等) は、エージェントの設定画面に理由と再試行ボタンのついた通知が出て、kojo は起動のたびにもう一度試す。

手作業で移行する場合:

1. パッケージをインストールし、エージェントで有効にする (2 章)
2. 組み込み版で使っていたのと **同じ** App-Level Token と Bot Token を入れ、4 つのスイッチを元と同じにする。Slack アプリ側の変更は要らない。スコープ、イベント、Interactivity の設定は、組み込み版が必要としたものと同じ
3. 組み込み bot をまだ含む kojo では、そのエージェントの組み込み bot がオフになっていることを確かめる。1 つのアプリに 2 本の接続があると、それぞれにイベントの一部しか届かない

引き継がれるもの:

- **スレッドのセッション。** スレッドは今も `<agent>:slack:<channel>:<thread>` というセッションなので、古いスレッドに返信すれば、進行中の goal も含めて、エージェントがそこでしていた会話の続きになる
- **設定項目** は名前も意味も同じ: `threadReplies`, `respondDM`, `respondMention`, `respondThread`
- **ツール** は名前も引数も同じ。MCP サーバーの名前も `slack` のまま
- **コマンドとメッセージ** (`!stop`, `!stop all`, `!goal`, 質問カード、各種の通知) は変わらない

自動では引き継がれないもの:

- **bot がどのスレッドを追っているかの記録。** これはファイルとして保存されている。以前はエージェントのデータディレクトリの `chat_history/slack/` に、今はこの拡張のデータディレクトリに同じ構成 (`chat_history/slack/<channel>/<thread>.jsonl`) で置かれる。kojo がエージェントを移行するときにはコピーされる。手作業で移行してコピーしなかった場合、古いスレッドは一度 @メンションされるまで自分のスレッドとして認識されない。メンションの後は、またメンションなしで続きに反応する。どちらの場合も、エージェントが覚えていることは失われない
- kojo の更新時に **実行中だったターン。** メッセージを送り直す

## うまくいかないとき

| 症状 | 確認すること |
| --- | --- |
| bot がオンラインにならない | トークンは 2 つとも保存したか。拡張とエージェントの両方が有効か。何が悪いかは kojo のログに出る: `the app-level token and the bot token are both required`、`appToken must start with xapp-` / `botToken must start with xoxb-` (2 つを逆に貼ったか、別のものを貼った)、`Slack refused the bot token` (アプリを再インストールし、トークンをコピーし直す) |
| `Slack accepted the bot token` の後、ログに接続エラーが出る | App-Level Token が間違っているか `connections:write` がない、または Socket Mode がオフ (1-2) |
| DM に返信しない | `message.im` を購読しているか (1-4)。Messages Tab は有効か (1-6)。"Answer direct messages" はオンか |
| メンションに返信しない | `app_mention` を購読しているか。bot をチャンネルに招待したか (1-8)。"Answer @mentions in channels" はオンか |
| スレッドの続きに返信しない | `message.channels` / `message.groups` を購読し、対応する `*:history` スコープがあるか。"Keep answering in threads" はオンか。メッセージがほかの人をメンションしていないか |
| メッセージの書き手が名前でなくユーザー ID になる | `users:read` を追加して再インストールする |
| 添付ファイルがエージェントに渡らない | `files:read` を追加して再インストールする |
| 返信がストリーミングされず、まとめて出る | Slack がその会話へのストリーミングを拒否したので、bot は完成した返信を投稿する方法に切り替えた。内容は欠けない |
| 回答する ボタンを押しても何も起きない | Interactivity をオンにする (1-5) |
| ツールが `missing_scope` で失敗する | そのツールに必要なスコープ (1-3) を追加して再インストールする |
| ツールが `not_in_channel` で失敗する | bot をチャンネルに招待するか、`chat:write.public` を追加する |
| スコープを変えたのに何も変わらない | 新しいスコープは、アプリを再インストールして初めて有効になる |

ログは kojo 本体のログに `extension=slack` のタグ付きで出る。

## 詳しい資料

- [docs/slack-errors.md](docs/slack-errors.md) — 失敗したターンが Slack でどう報告されるか
- [docs/slack-questions.md](docs/slack-questions.md) — 質問カードの準備、ルール、実装メモ

## 制限

- スラッシュコマンドはない。コマンドは `!` で始まる通常のメッセージ
- ユーザーやチャンネルの許可リストはない
- メッセージの編集と削除は、エージェントに伝わらない
- ツールは、このパッケージがインストールされているデバイスで使える。別のデバイスへ移ったエージェントは、そのデバイスにもパッケージがインストールされている場合にだけツールを使える

## 開発

```sh
make test    # go vet + go test
make build   # 全プラットフォームのバイナリを bin/ にビルド
```

kojo は拡張パッケージを `git clone` でインストールし、ビルドはしない。そのためバイナリをリポジトリにコミットしている。コードを変えたら `make build` を実行し、`bin/` も一緒にコミットして、`kojo-package.json` の `version` を上げる。

同じバイナリが 2 つの役割を持つ。引数なしなら Slack に接続し続けるサービス、`mcp` を付けると stdio の MCP サーバーになる。

| ディレクトリ | 内容 |
| --- | --- |
| `internal/slackbot` | bot 本体: Socket Mode のイベント、スレッドごとの順序、ストリーミング、コマンド、質問 |
| `internal/kojoclient` | bot がターンを実行するのに使う、kojo の `ext-chat` API のクライアント |
| `internal/chathistory` | スレッドごとの履歴ファイル |
| `internal/agent`, `internal/uploadpath` | kojo と共有する型とパス |

### 偽の Slack を相手にテストする

テスト専用として、環境変数 `EXT_SLACK_API_URL` で Slack の Web API のアドレスを差し替えられる (例: `http://127.0.0.1:9000/api/`)。サービスも MCP サーバーも起動時に一度だけ読み、Socket Mode の接続先はそのサーバーが `apps.connections.open` に返したアドレスになる。kojo は決まった環境変数だけを渡してパッケージを起動するので、設定するにはテスト用にコピーした `kojo-package.json` の `contributes.service` と MCP サーバーの `env` に書く。通常の利用では設定しないこと。設定すると、トークンが Slack ではなくそのアドレスに送られる。

## ライセンス

MIT
