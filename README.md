# dth-msg
もし仮に死んでしまった場合、メッセージの送信ができるようにするプログラム

Google Health APIからFitbitの心拍数と端末状態を定期取得し、心拍数の欠落が設定時間を超えると、登録したメッセージをDiscord・Slack・Mastodonへ送信する単一ユーザー用Goサーバーです。Cloudflare Workersでは同じGoコードをWebAssemblyとして実行します。外部Goサーバーは不要です。

**心拍数の欠落だけでは死亡を医学的に確認できません。** 装着忘れ・同期不良・センサー故障でもデータは欠落します。本サービスは「死亡の可能性」の条件判定であり、救急・医療・安否確認の代替ではありません。自動投稿の内容・公開範囲・送信先には十分注意してください。

## 判定ルール

- 初回起動後に有効な心拍数を受信するまで判定しません。新しい心拍数を受信すると欠落時間と配信済みフラグをリセットします。
- 前回・今回とも監視可能であるチェック間隔だけを欠落時間に加算します。既定値は累積24時間です。
- 対象端末の`batteryStatus=Empty`または`batteryLevel=0`の期間は加算せず、新たな判定を行いません。充電後の最初のチェック間隔も除外します。
- 不明なバッテリー状態、古い/未来の同期時刻、対象端末不在、APIエラー（429を含む）は判定を停止します。APIエラーや停止期間は後から加算しません。
- 監視自体が停止した長い間隔（既定15分超）も除外します。APIの再取得は次の5分間隔チェックで行い、即時連続リトライはしません。
- バッテリー残量は最後の同期情報であり、APIには電池切れの開始・終了履歴がありません。**未同期の電池切れ期間を完全に特定することはできません。** 既定では最終同期から120分を超えると停止します。適切に同期し続けないと送信されません。
- 心拍数はGoogle/Fitbitウェアラブル由来のデータだけを読みます。同じGoogleアカウントの複数ウェアラブルのデータは区別しません。監視するFitbitのバッテリーは`FITBIT_DEVICE_NAME`で明示的に選択します。
- 判定結果と送信先ごとの成功を永続化し、成功した送信先には同一イベントを再送しません。成立済みイベントの失敗した送信先は、後の電池切れ・同期停止・API障害中でも次回チェックで再試行します。新しい心拍数を受信した場合はイベントを解除します。SNSの受理直後にプロセスが停止すると、保存前の送信は重複する可能性があります。Mastodonには同じイベントの冪等キーを付与しますが、Discord/Slackのexactly-once配信は保証できません。

## Google Health APIの準備

Google CloudプロジェクトでGoogle Health APIを有効にし、OAuth同意画面とOAuthクライアントを設定してください。アプリに必要なアクセス承認・検証も行ってください。

所有者のGoogleアカウントで次のスコープを許可し、OAuth 2.0 Authorization Codeフローの`access_type=offline`（初回は`prompt=consent`）でリフレッシュトークンを取得します。トークン発行/再認可のUIは本サーバーに含みません。Google OAuth Playgroundを使う場合は自分のOAuthクライアントを指定してください。

```text
https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly
https://www.googleapis.com/auth/googlehealth.settings.readonly
```

リフレッシュ時に返された新しいリフレッシュトークンは状態ストアに保存します。失効した場合は再認可し、状態ストア内の古いトークンも管理者が削除・更新してください（状態全体の削除は監視時間と配信履歴もリセットします）。

利用するAPI:

- `GET https://health.googleapis.com/v4/users/me/pairedDevices` — `TRACKER`の`name`を`FITBIT_DEVICE_NAME`に設定。ページがある場合は全ページを確認。
- `GET https://health.googleapis.com/v4/users/me/dataTypes/heart-rate/dataPoints` — `filter`によるUTC時間範囲、`dataSourceFamily=users/me/dataSourceFamilies/google-wearables`、全ページ取得。
- `POST https://oauth2.googleapis.com/token` — チェック時にアクセストークンを更新。

心拍数は初回に直近24時間、その後は直近チェックから1時間の重複を含む範囲を取得します。範囲外まで遅れた同期データは検出できません。Googleの現行APIを使用し、旧Fitbit Web APIやGoogle Fit APIには依存しません。

公式資料: [Google Health](https://developers.google.com/health/about)、[心拍数データ](https://developers.google.com/health/reference/rest/v4/users.dataTypes.dataPoints/list)、[端末](https://developers.google.com/health/reference/rest/v4/users.pairedDevices)、[OAuth](https://developers.google.com/identity/protocols/oauth2/web-server)、[WorkersのWasm](https://developers.cloudflare.com/workers/runtime-apis/webassembly/javascript/)。

## 設定

| 環境変数 | 内容 / 既定値 |
|---|---|
| `ADMIN_TOKEN` | 管理APIのBearerトークン。32バイト以上のランダムな値（例: `openssl rand -hex 32`） |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | OAuthクライアント |
| `GOOGLE_REFRESH_TOKEN` | 所有者のリフレッシュトークン |
| `FITBIT_DEVICE_NAME` | APIが返す端末の完全なリソース名。コンソールから登録する場合は省略可 |
| `SNS_TARGETS_JSON` | 下記の初期送信先配列。最大10件、IDは重複不可。コンソールから登録する場合は省略可 |
| `MISSING_HOURS` | 欠落時間。既定`24`、1〜8760時間 |
| `MAX_SYNC_AGE_MINUTES` | 同期情報の最大経過時間。既定`120`、1〜1440分 |
| `MAX_POLL_GAP_MINUTES` | 加算するチェック間隔の上限。既定`15`、1〜1440分。通常は5分より大きく設定 |
| `STATE_FILE` | ネイティブ版のみ。既定`state.json` |
| `LISTEN_ADDR` | ネイティブ版のみ。既定`127.0.0.1:8080` |

`SNS_TARGETS_JSON`の形式（URL・トークンは実際の値に置換）:

```json
[
  {"id":"discord","kind":"discord","url":"https://discord.com/api/webhooks/WEBHOOK_ID/WEBHOOK_SECRET"},
  {"id":"slack","kind":"slack","url":"https://hooks.slack.com/services/WORKSPACE/CHANNEL/WEBHOOK_SECRET"},
  {"id":"mastodon","kind":"mastodon","url":"https://YOUR_INSTANCE/api/v1/statuses","token":"MASTODON_ACCESS_TOKEN"}
]
```

Mastodonには`write:statuses`権限を持つトークンが必要です。Discord/Slackは事前作成したWebhookを使います。送信先設定は管理者だけが変更できるシークレットとして扱い、Mastodonには信頼する公開インスタンスのみ設定してください。リダイレクトは禁止します。Discordの自動メンションは無効です。X・LINE等は未対応です。

## Goサーバーとして起動

Go 1.24以上が必要です。上記必須環境変数を安全な方法で設定後:

```sh
make test
make build
./build/dth-msg
```

起動時および5分ごとにチェックします。状態ファイルは0600で保存します。信頼できる専用ディレクトリ/永続ボリュームを使い、**同じ状態ファイルを複数プロセスから共有しないでください**。HTTPSリバースプロキシの背後で運用し、管理APIを公開する場合はアクセス制限も追加してください。メッセージを登録するまで送信しません。

## Cloudflare Workersへデプロイ

GoとNode.js/npm、Workersアカウントが必要です。JavaScriptはHTTP・Cron・Durable ObjectストレージとGo Wasmを接続するだけで、API取得・判定・SNS配信・管理APIはGoで実行します。Cronは5分ごと、Durable Objectはユーザーの状態を保存し、チェックと管理操作を直列化します。

```sh
npx wrangler@4.158.0 login
npx wrangler@4.158.0 secret put ADMIN_TOKEN
npx wrangler@4.158.0 secret put GOOGLE_CLIENT_ID
npx wrangler@4.158.0 secret put GOOGLE_CLIENT_SECRET
npx wrangler@4.158.0 secret put GOOGLE_REFRESH_TOKEN
npx wrangler@4.158.0 secret put FITBIT_DEVICE_NAME
npx wrangler@4.158.0 secret put SNS_TARGETS_JSON
npx wrangler@4.158.0 deploy
```

時間設定は`wrangler.toml`の`vars`で変更できます。ビルドは`make wasm`で自動実行します。Goランタイムは使用中のGoに付属する`wasm_exec.js`をコピーし、第三者ライブラリはGo側に不要です。Wasmとビルド成果物はGit管理しません。WorkersのCPU・メモリ・圧縮コードサイズ・外部リクエスト数のプラン制限を確認してください。大量のページ取得や送信先では有料プランが必要になる場合があります。

ローカル確認は、上記変数を設定したGit管理対象外の`.dev.vars`を用意して`npx wrangler@4.158.0 dev --local`。デプロイせずバンドルだけ確認するには`npx wrangler@4.158.0 deploy --dry-run`。実際のGoogle認可・SNS配信には自分の資格情報が必要です。

## メッセージ登録・状態確認

### 管理コンソール

ブラウザでサーバーの `/`（ローカルは `http://127.0.0.1:8080/`）を開き、`ADMIN_TOKEN`で接続します。Go版・Workers版の両方で利用できます。GoogleのOAuth資格情報と管理トークンは引き続き環境変数で設定してください。端末と送信先の環境変数は省略でき、未登録の間は監視しません。Workersでも該当する2つの`secret put`は省略できます。

- 対象Fitbitのリソース名を登録します（Google Health APIの`pairedDevices`が返す`TRACKER`の`name`）。対象は1台です。端末の自動取得・OAuth認可画面は含みません。
- Discord・Slack・Mastodonの送信先と各メッセージを最大10件登録できます。空のメッセージの送信先には配信しません。Webhook URL・Mastodonトークンは画面ではマスクします。
- 保存は端末・送信先・メッセージをまとめて置き換え、状態ストアに永続化します。保存した設定は環境変数の初期値より優先され、再起動後も有効です。保存操作自体は配信を行いません。
- 端末変更時は監視の欠落時間と心拍の基準をリセットします。判定成立中は設定を変更できません。
- 最終心拍・最終チェック・欠落時間・停止理由・配信済みIDを確認できます。

管理トークンはブラウザのメモリ内だけで保持し、ログアウト・再読み込みで破棄します。画面とスクリプトは未認証でも取得できますが、設定・状態の読み書きにはBearer認証が必要です。`GET /settings`は認証した管理者に送信先の秘密情報とメッセージを返すため、HTTPSと管理画面へのアクセス制限を使用してください。設定は既存の状態ファイル（0600）またはDurable Objectストレージに保存します。

### 管理API

`GET /settings`で設定を取得、`PUT /settings`で`{"deviceName":"users/me/pairedDevices/…","targets":[…],"messages":{"送信先ID":"本文"}}`を保存できます。Bearer認証が必要です。

`BASE_URL`（ローカルなら`http://127.0.0.1:8080`、WorkerならデプロイURL）と`ADMIN_TOKEN`を設定して実行します。以下は登録した全メッセージを置き換えます。登録しない送信先には配信しません。

```sh
curl --fail-with-body -X PUT "$BASE_URL/messages" \
  -H "$(printf 'Authorization: %s %s' Bearer "$ADMIN_TOKEN")" \
  -H 'Content-Type: application/json' \
  --data '{"discord":"皆さん、今までありがとうございました。","slack":"登録したメッセージ"}'

curl --fail-with-body "$BASE_URL/status" -H "$(printf 'Authorization: %s %s' Bearer "$ADMIN_TOKEN")"
curl --fail-with-body -X POST "$BASE_URL/check" -H "$(printf 'Authorization: %s %s' Bearer "$ADMIN_TOKEN")"
```

メッセージは非空、Discord/Slackは2000文字、Mastodonは500文字までです（実際のインスタンス制限は別途適用）。判定成立後は内容をロックし、新しい心拍数を受信するまで変更できません。`/status`は最終心拍時刻・欠落秒数・停止理由・`suspectedDeath`・配信済みIDのみ返し、トークンやメッセージを返しません。すべての管理APIで認証が必要です。

## テスト

```sh
go test ./internal/monitor -count=1
make test
go test -race ./...
make build wasm
```

実APIや実SNSに送信せず、電池切れ・古い同期・初回心拍待ち・心拍回復・監視停止・APIエラー・ページング・配信再試行・永続化・認証をテストします。
