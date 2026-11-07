# dth-msg
もし仮に死んでしまった場合、メッセージの送信ができるようにするプログラム

Google Health APIからFitbitの心拍数と端末状態を定期取得し、心拍数の欠落が設定時間を超えると、登録したメッセージをDiscord・Slack・Mastodon・X（任意）へ送信する単一ユーザー用Goサーバーです。Cloudflare Workersでは同じGoコードをWebAssemblyとして実行します。外部Goサーバーは不要です。

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

## 1人用のClerkログイン（推奨）

公式Go SDK `github.com/clerk/clerk-sdk-go/v2` とClerkJSを使用します。ログイン・Google/Xの認可情報はClerk、端末・送信先・メッセージ・監視履歴は既存ストアで保持し、追加のDBは不要です。Googleの資格情報を手動発行する従来方式も利用できます。

1. Clerkでアプリを作り、Google接続を有効にします。最初にClerkのAccount Portalから自分のユーザーを作成し、その `user_…` を `OWNER_CLERK_USER_ID` に設定してください。アプリ側ではこのIDだけを許可します。Clerk側でもサインアップを自分のメールアドレスに制限することを推奨します。
2. Google CloudでGoogle Health APIとWebアプリ用OAuthクライアントを設定し、そのID・シークレットをClerkのGoogle接続の **Use custom credentials** に登録します。リダイレクトURIはClerkが表示する値をGoogle Cloudに登録してください。Clerkの共有開発用資格情報では追加スコープ・プロバイダートークン取得が使えません。
3. ClerkのGoogle接続のScopesとGoogle Cloudの同意画面に、次の2スコープを追加します。既に連携したアカウントは再接続して許可してください。Clerkが保持する更新用トークンによるオフラインアクセスが必要です。

   ```text
   https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly
   https://www.googleapis.com/auth/googlehealth.settings.readonly
   ```

4. 下表のClerk用環境変数と `ADMIN_TOKEN` を設定します。`CLERK_JWT_KEY` はClerkのAPI keys画面の **JWKS Public Key**（PEM形式、実際の改行を保持）です。秘密鍵ではありません。`PUBLIC_URL` はブラウザで開くorigin（例 `https://dth-msg.example.com`、ローカルは `http://127.0.0.1:8080`）。末尾の `/` は付けません。
5. `/` を開いてClerkでログインし、「Fitbit端末を取得」から対象を選び、送信先・メッセージを保存します。Googleの認可情報はClerk側に永続保存され、5分ごとのチェック時にGo SDKから取得・更新します。ブラウザを閉じたりアプリが再起動しても監視は続きます。

Clerk利用時は `GOOGLE_CLIENT_ID`・`GOOGLE_CLIENT_SECRET`・`GOOGLE_REFRESH_TOKEN` のアプリへの設定は不要です。Clerkに設定したGoogle接続を使い、従来のGoogle環境変数にはフォールバックしません。ClerkやGoogleの認可に問題がある場合は監視を停止し、停止時間は加算しません。所有者のGoogle接続は1件にしてください（複数件なら誤選択を避けるため停止します）。

初めてClerk方式へ移行した場合やGoogle接続アカウントを変更した場合は、心拍の基準・欠落時間をリセットします。判定成立中に異なるGoogle接続へ切り替えた場合は、その接続での監視を停止して既存イベントの配信処理を維持します。判定成立中の認可方式変更・接続アカウント変更は避けてください。

ClerkのブラウザセッションとGoogleの更新用トークンは別です。ログアウトやブラウザセッション満了で監視は止まりません。Google HealthのTesting設定では更新用トークンが7日で失効するため、Clerkに保存してもこの制限は変わりません。長期運用にはGoogle側の公開設定・必要な審査を確認してください。個人利用でも審査免除や永久有効は保証できません。

JWTは公開鍵でローカル検証し、署名・有効期限・issuer・origin（azp）・所有者IDを確認します。公開鍵をローテーションした場合は `CLERK_JWT_KEY` も更新してください。APIにはブラウザが取得した短期トークンをBearerで渡し、変更操作は `PUBLIC_URL` と一致するOriginを要求します。`ADMIN_TOKEN` は従来の管理APIとCron用に残します。状態ストアにも所有者IDを記録し、IDだけを変更して既存データを別人に引き継ぐことはできません。

### Discord連携

Discord Developer PortalでOAuthアプリを作成し、リダイレクトURIに `PUBLIC_URL` + `/integrations/discord/callback` を登録します。`DISCORD_CLIENT_ID`・`DISCORD_CLIENT_SECRET` を設定すると、「Discordの投稿先を選ぶ」ボタンが表示されます。Discordでチャンネルを選ぶと、`webhook.incoming` フローで取得したWebhookを既存ストアに保存します。ClerkへのDiscord接続や常時接続Botは不要です。

認可処理は有効期限10分・一度限りのstateとHttpOnly Cookieで開始したブラウザに紐付けます。キャンセル時は送信先を変更しません。Discordユーザー用アクセストークンは保持しません。再連携すると新しいWebhookが作られるため、不要なWebhookはDiscord側で削除してください。追加後にメッセージを登録するまで配信しません。

### X連携（任意・既定で無効）

1. Clerkに **X/Twitter v2（OAuth 2.0）** 接続を追加し、自分のXアプリの資格情報を設定します。XアプリはRead and Writeを許可し、Clerkのスコープに `tweet.write`・`offline.access` を追加します（Clerkの標準スコープも維持）。旧TwitterのOAuth 1.0接続は使用しません。
2. `ENABLE_X=true` を設定します。「アカウント設定」でXを連携し、「Xを送信先に追加」を押してメッセージを保存します。所有者の1アカウントだけに対応し、IDは `x`、API URLは固定です。
3. X Developer Consoleで利用料・クレジット残高・支出上限を確認してください。このアプリから課金契約や自動チャージの設定は行いません。

通常の監視ではXのAPIもClerkのXトークンAPIも呼びません。判定成立かつX向けメッセージがある場合だけトークンを取得して投稿します。費用と重複の拡大を避けるため、投稿試行回数を送信前に保存し、1イベント最大3回で自動再試行を停止します（失敗・応答不明・送信直前の停止も含む）。回数は状態画面で確認できます。新しい心拍を受信した次のイベントではリセットします。Xもexactly-onceは保証できません。文字数は重み付き計算への対応を小さく保つため、保守的に140 Unicode文字までです。リンク展開などX側の制約で失敗する場合もあります。

### 費用と参考資料

Clerk Hobbyの無料プランで、1人用のログインとGoogle・Xの2つのソーシャル接続を想定しています。別の認証サーバー・DB・常駐Botは追加しません。ホスティング費用は既存のGoサーバーまたはWorkersの利用量・CPU・コードサイズ上限に依存し、無料枠で動くことは保証しません。

Xは従量課金です。2026-11-07確認の公式表では通常投稿 $0.015/回、URL付き投稿 $0.20/回です。認可フローでプロバイダーがユーザー情報を取得する可能性もあり、初回接続が無料とは限りません。Google Healthの公開時の審査・セキュリティ評価も別途確認が必要です。

公式資料: [Clerk Go SDK](https://clerk.com/docs/reference/go/overview)、[外部トークン管理](https://clerk.com/docs/go/guides/configure/auth-strategies/social-connections/overview)、[Clerk料金](https://clerk.com/pricing)、[Discord OAuth](https://docs.discord.com/developers/topics/oauth2#webhooks)、[ClerkのX連携](https://clerk.com/docs/guides/configure/auth-strategies/social-connections/x-twitter)、[X料金](https://docs.x.com/x-api/getting-started/pricing)、[Google Healthの設定と有効期限](https://developers.google.com/health/setup)、[Google Healthの審査](https://developers.google.com/health/app-verification)。

## Google Health APIの準備（Clerkを使わない場合）

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
| `PUBLIC_URL` | Clerk利用時必須。ブラウザで開くorigin。HTTPS（ローカルのみHTTP可）、末尾スラッシュなし |
| `CLERK_PUBLISHABLE_KEY` | ClerkのPublishable Key。設定するとClerkモードを有効化 |
| `CLERK_JWT_KEY` | ClerkのJWKS Public Key（RSA 2048ビット以上のPEM形式） |
| `CLERK_SECRET_KEY` | 外部プロバイダートークン取得用のClerk Secret Key |
| `OWNER_CLERK_USER_ID` | 自分のClerkユーザーID（`user_…`）。Clerk利用時必須 |
| `DISCORD_CLIENT_ID` / `DISCORD_CLIENT_SECRET` | 任意。Discordの投稿先選択OAuth用 |
| `ENABLE_X` | `true`のときX連携・投稿を有効化。既定は無効 |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | Clerkを使わない場合のOAuthクライアント |
| `GOOGLE_REFRESH_TOKEN` | Clerkを使わない場合の所有者リフレッシュトークン |
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

Mastodonには`write:statuses`権限を持つトークンが必要です。Discord/SlackはWebhookを使います（Discordは上記OAuthで作成可能）。送信先設定は管理者だけが変更できるシークレットとして扱い、Mastodonには信頼する公開インスタンスのみ設定してください。リダイレクトは禁止します。Discordの自動メンションは無効です。LINE等は未対応です。

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

Clerkモードでは上のGoogle用3シークレットの代わりに `CLERK_PUBLISHABLE_KEY`・`CLERK_JWT_KEY`・`CLERK_SECRET_KEY`・`OWNER_CLERK_USER_ID`・`PUBLIC_URL` を `secret put` で設定してください。端末と送信先は画面から登録できます。Discord連携を使う場合はDiscord用2シークレット、Xを使う場合は `ENABLE_X=true` も設定します。

時間設定は`wrangler.toml`の`vars`で変更できます。ビルドは`make wasm`で自動実行します。Goランタイムは使用中のGoに付属する`wasm_exec.js`をコピーします。Go側の依存ライブラリはClerk公式SDKとその依存です。Wasmとビルド成果物はGit管理しません。WorkersのCPU・メモリ・圧縮コードサイズ・外部リクエスト数のプラン制限を確認してください。大量のページ取得や送信先では有料プランが必要になる場合があります。

ローカル確認は、上記変数を設定したGit管理対象外の`.dev.vars`を用意して`npx wrangler@4.158.0 dev --local`。デプロイせずバンドルだけ確認するには`npx wrangler@4.158.0 deploy --dry-run`。実際のGoogle認可・SNS配信には自分の資格情報が必要です。

## メッセージ登録・状態確認

### 管理コンソール

ブラウザでサーバーの `/`（ローカルは `http://127.0.0.1:8080/`）を開き、ClerkモードではClerkで、従来モードでは `ADMIN_TOKEN` で接続します。Go版・Workers版の両方で利用できます。端末と送信先の環境変数は省略でき、未登録の間は監視しません。

- 「Fitbit端末を取得」から対象を選択するか、対象Fitbitのリソース名を入力します（Google Health APIの`pairedDevices`が返す`TRACKER`の`name`）。対象は1台です。
- Discord・Slack・Mastodonの送信先と各メッセージを最大10件登録できます。空のメッセージの送信先には配信しません。Webhook URL・Mastodonトークンは画面ではマスクします。
- 保存は端末・送信先・メッセージをまとめて置き換え、状態ストアに永続化します。保存した設定は環境変数の初期値より優先され、再起動後も有効です。保存操作自体は配信を行いません。
- 端末変更時は監視の欠落時間と心拍の基準をリセットします。判定成立中は設定を変更できません。
- 最終心拍・最終チェック・欠落時間・停止理由・配信済みIDを確認できます。

従来モードの管理トークンはブラウザのメモリ内だけで保持し、ログアウト・再読み込みで破棄します。Clerkのログイン状態はClerkが管理し、API呼び出しごとに短期トークンを取得します。画面・スクリプト・公開用ログイン設定は未認証で取得できますが、設定・状態の読み書きにはBearer認証が必要です。`GET /settings`は認証した管理者にWebhookなどの秘密情報とメッセージを返すため、HTTPSと管理画面へのアクセス制限を使用してください。Google・Xのプロバイダートークンは返しません。設定は既存の状態ファイル（0600）またはDurable Objectストレージに保存します。

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

メッセージは非空、Discord/Slackは2000文字、Mastodonは500文字、Xは140文字までです（実際のサービス側制限は別途適用）。判定成立後は内容をロックし、新しい心拍数を受信するまで変更できません。`/status`は最終心拍時刻・欠落秒数・停止理由・`suspectedDeath`・配信済みID・X投稿試行回数を返し、トークンやメッセージを返しません。管理APIはBearer認証、Discordの認可コールバックは一度限りのstateとブラウザCookieで保護します。

## テスト

```sh
go test ./internal/monitor -count=1
make test
go test -race ./...
make build wasm
node --test worker/bridge_test.mjs
node worker/wasm_test.mjs build/monitor.wasm "$(go env GOROOT)/lib/wasm/wasm_exec.js"
```

実APIや実SNSに送信せず、電池切れ・古い同期・初回心拍待ち・心拍回復・監視停止・APIエラー・ページング・配信再試行・永続化・認証をテストします。Clerkの署名・所有者・origin検証、外部トークンの権限、Google接続変更、Discordのstate/Cookie・再利用・失効、Xの投稿回数上限、Workersのヘッダー引き継ぎもテストします。実際のClerk・Google・Discord・Xとの認可には各アプリの設定・資格情報が必要です。
