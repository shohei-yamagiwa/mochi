# GitHub App としてのデプロイ

この手順は `develop` から `main` にマージされた実装（基点 `28de02c`）に基づく。
この準備変更も `main` にマージしてから公開する。`render.yaml` のデプロイ対象は `main`。

## 推奨する初期構成

**Render Starter 1 台 + 永続ディスク 1 GB + SQLite + Daytona + Gemini** を推奨する。
独自ドメインは不要で、Render の HTTPS URL を GitHub の Webhook に設定できる。
サーバー上では Go のオーケストレーターを常駐させ、コードの実行は Daytona が受け持つ。

| 候補 | 向いている場合 | このアプリで必要なこと |
| --- | --- | --- |
| **Render（推奨）** | 月々の基礎費用を把握しやすく、設定を Git 管理したい | 同梱 Blueprint、Starter、永続ディスク、1 インスタンス |
| Railway | 既に Railway を使っている、従量課金が好み | Docker、Volume を `/data` に接続、スリープ無効、1 インスタンス、同じ秘密情報 |
| 小さい VPS | 自前サーバーを保守できる | Compose に加え、HTTPS、OS 更新、バックアップ、監視を自分で運用 |

2026-11-15 に確認した [Render 料金](https://render.com/pricing)では、Starter は
**7 USD/月、ディスクは 0.25 USD/GB/月**。1 GB なら基礎費用は約 **7.25 USD/月**。
税・為替・転送量などは別。契約時に現在の料金を確認する。
**Gemini と Daytona の利用料は含まれない**。レビュー頻度、PR サイズ、ターン数、
VM 稼働時間によって増えるため、各サービスの使用量と予算通知も設定する。

無料のスリープするサービスやリクエストの間だけ動く関数は、このままの構成には適さない。
Webhook の応答後に非同期レビューを続け、SQLite とメモリー内キューを使うためである。
複数インスタンスやスケールゼロへの対応には、外部 DB・永続ジョブキューなどへの変更が必要。

## 最初の利用範囲と費用

同梱の `configs/workflows/pr-review-small.yaml` は、既存の correctness プロンプトを
使う専門エージェント 1 つと synthesizer に絞る。4 専門分野のフルレビューではない。
Gemini のキーだけで LLM を設定でき、サーバーの同時実行数は 1、待ち行列は 16。
Daytona では通常、ベースと専門エージェント用の計 2 サンドボックスが必要になる。
監査データや PR の大きさによって 512 MB RAM が不足する場合はプランを上げる。

GitHub App は **Pull request と Issue comment の両方**を購読する。
small プリセットは PR の作成（`opened`）、コミット追加（`synchronize`）、
再オープン（`reopened`）、Draft 解除（`ready_for_review`）で自動実行する。
Draft 中は実行せず、PR は open であることが必要。
`/mochi review` のコメントでも再実行できる。コメント投稿者は
OWNER / MEMBER / COLLABORATOR に限定される。
手動実行だけにする場合は App の Pull request 購読を外す。
**空の `events: []` は無効化ではなく既定イベントに戻る**。
専門エージェントと synthesizer はどちらも `gemini-3.8-flash` を使用する。
Gemini API のプロジェクトでモデルの利用権限とクォータを確認する。
別のモデルに変更する場合は YAML の specialist と synthesizer の両方を変更する。
トークン予算は呼び出し後の計数なので、厳密な金額上限ではない。

## 1. GitHub App を作る

GitHub の個人または Organization の **Settings → Developer settings → GitHub Apps → New GitHub App**
から作成する。名前は GitHub 全体で一意なものにする。

| 設定 | 値 |
| --- | --- |
| Homepage URL | `https://github.com/shohei-yamagiwa/mochi` |
| Webhook URL | 最終的に `https://<service>.onrender.com/webhooks/github` |
| Webhook secret | `openssl rand -hex 32` で生成した値 |
| SSL verification | 有効 |
| Callback URL / OAuth | 不要（ユーザー OAuth は実装していない） |
| Where can this GitHub App be installed? | 初回は Only on this account |

まだサービス URL がなければ Webhook の Active を一時的に外して App を作成し、
URL 入力が必要なら `https://example.com/webhooks/github` を一時値にする。
デプロイ後に本物の URL に更新して Active を有効にする。

Repository permissions:

| 権限 | 必要なレベル | 用途 |
| --- | --- | --- |
| Metadata | Read（自動付与） | リポジトリ情報 |
| Contents | Read | PR ソースの取得、読み取り専用 clone token |
| Pull requests | Read and write | PR 情報とレビュー・インラインコメント投稿 |
| Checks | Read and write | Check Run の作成と更新 |
| Issues | Read | Issue comment イベントの購読 |

イベント購読は **Pull request と Issue comment の両方**を有効にする。
Actions、Administration、Contents write は不要。
App ID（Client ID とは別）を控え、Private keys から RSA 秘密鍵を生成・ダウンロードする。
App の **Install App** から、まずテスト対象の 1 リポジトリだけにインストールする。
Installation ID は Webhook から取得するためサーバー環境変数には不要。
権限を後から変更した場合は、インストール先で更新の承認も行う。

## 2. Daytona の準備

Daytona のアカウントと API キーを作成する。`deploy/sandbox/Dockerfile` を元に
Daytona の Snapshot 作成画面または CLI で **`mochi-review`** という Snapshot を作る。
対象サーバーと近いリージョン/target を選び、Snapshot が利用可能な状態になるまで待つ。
名称を変える場合は small プリセットの `sandbox.default_snapshot` も変更する。

この Snapshot には git、bash、ripgrep、CA 証明書を含む。対象プロジェクトのテストを
実行するには、その言語ランタイムやビルドツールも Dockerfile に追加して Snapshot を更新する。
**サーバー用のルート Dockerfile と、Daytona 用 Dockerfile は別物**。
GitHub・LLM・Daytona の秘密情報を Snapshot に埋め込まない。

実装はサンドボックスを削除し、15 分の自動停止も設定する。ただしサーバー異常終了時の
外部リソースの削除は保証できないため、Daytona 側の使用量と残存サンドボックスも確認する。

## 3. 環境変数

秘密情報はホスティングの環境変数 / Secret に登録する。チャットや Git に貼らない。
ローカルでは `.env.example` を `.env` にコピーして `chmod 600 .env` を行い、値を埋める。
`.env`・PEM・DB は Git と Docker ビルドコンテキストから除外している。

| 変数 | 初期構成での用途 |
| --- | --- |
| `MOCHI_GITHUB_APP_ID` | GitHub App の数値 App ID |
| `MOCHI_GITHUB_PRIVATE_KEY` | ダウンロードした PEM 全文、実際の改行を含む |
| `GITHUB_WEBHOOK_SECRET` | GitHub App と同じランダムな secret |
| `MOCHI_API_TOKEN` | 検査 API と汎用 trigger API 用の別のランダム値 |
| `DAYTONA_API_KEY` | Daytona キー |
| `GEMINI_API_KEY` | small プリセットの Gemini API キー（`GOOGLE_API_KEY` でも可） |
| `PORT` | コンテナの待受ポート（既定 8080、入口スクリプトが反映） |
| `MOCHI_DB_PATH` | `/data/mochi.db`（永続ディスク内） |
| `MOCHI_WORKFLOW` | `configs/workflows/pr-review-small.yaml` |
| `MOCHI_WORKERS` / `MOCHI_QUEUE_SIZE` | 初期値 1 / 16 |

`openssl rand -hex 32` を別々に実行して Webhook secret と API token を作る。
Render の Blueprint では API token を自動生成し、それ以外の秘密情報は入力を求める。
Render へ PEM を貼る際は `.env` 用の外側の引用符を含めない。
ファイル方式なら `MOCHI_GITHUB_PRIVATE_KEY_PATH` を使用できるが、
初期構成では inline PEM に統一する。両方式を同時に設定しない。
GitHub App 運用では `GITHUB_TOKEN` / `GH_TOKEN` は設定不要。
小規模プリセットでは `OPENAI_API_KEY` は不要。
既存の Render サービスに `GEMINI_API_KEY` を登録済みなら、その値を引き続き使う。
両方の Gemini 用変数を登録した場合は `GEMINI_API_KEY` を優先する。

4 専門エージェント版に切り替える場合は `OPENAI_API_KEY` と `ANTHROPIC_API_KEY`
も登録し、フルプリセットが指定する Daytona Snapshot を作成する。

## 4. ローカルでコンテナを確認

```sh
docker compose build
docker compose run --rm mochi validate --runtime configs/workflows/pr-review-small.yaml
docker compose up -d
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
docker compose logs --tail=100 mochi
```

`validate --runtime` は YAML・プロンプト・必要な LLM/Daytona の環境変数・GitHub
認証設定・HTTP 認証設定をローカルに検証する。課金 API は呼ばない。
キーの失効、モデルの利用権限、Snapshot の存在、GitHub の実権限は確認しない。
入口スクリプトは serve の前にこの検証を自動実行する。

Compose は localhost にのみ公開する。GitHub から受信するには HTTPS のトンネルを
別途使うか、次の Render デプロイ後に確認する。Go バイナリ単体は `.env` を自動読込しない。

## 5. Render に公開

1. 準備変更を `main` にマージし、CI のテストとイメージビルドの成功を確認する。
2. Render で GitHub のこのリポジトリを接続し、**New → Blueprint** で `render.yaml` を選ぶ。
3. Starter と 1 GB ディスクの料金、配置リージョンを確認し、必要な秘密情報を入力する。
4. デプロイ後に `/readyz` が 200 であることを確認する。ログの private key / provider
   不足やディスク権限エラーがないことも確認する。ディスクは `/data`、インスタンスは 1。
5. GitHub App の Webhook URL を実際の HTTPS URL + `/webhooks/github` に変更し、Active を有効化する。
6. 次節の実 PR テストを行ってから対象リポジトリを広げる。

`autoDeployTrigger: off` にしてあるので、以降のリリースは明示的な Manual Deploy で行う。
新しい Secret を Blueprint に追加した場合、既存サービスには手動登録が必要になることがある。
サーバーはディスク初期化のために root で入口を起動し、`/data` の所有者を整えてから
**UID/GID 10001 に落としてアプリを実行**する。秘密情報を持つのはサーバー側であり、
LLM キーは Daytona の実行環境には渡さない。

## 6. 実接続の確認

最初に GitHub App の Advanced → Recent deliveries で ping を再配信し、200 の
`ignored` 応答を確認する。ping はレビュー実行を開始しない。

次に、小さな変更を含む non-draft のテスト PR を作成し、コメントなしで自動レビューが
始まることを確認する。コミット追加でも再実行され、Draft 中は実行されず、Draft を
解除すると実行されることを確認する。必要に応じてリポジトリ所有者などが
`/mochi review` とコメントして手動でも再実行できる。
これらの確認は実際に LLM / Daytona を利用し、PR に書き込む。

- Webhook が 202 を返すこと。
- PR に Mochi の Check Run とレビューが作られること。
- レビュー対象 SHA と coverage、未確認ファイルの表示を確認すること。
- 成功後、Daytona のベース / fork サンドボックスが削除されること。
- 認証なしの `GET /api/v1/runs` が 401、API token を付けたリクエストが 200 になること。
- 再デプロイ後も過去の run を API / `mochi inspect --db /data/mochi.db` で確認できること。

`/readyz` の成功は SQLite への接続を含むサーバーの準備状態であり、外部 API の成功保証ではない。
`202` は受信承認であり、レビュー完了ではない。GitHub 情報取得や後続実行の失敗は
ログと Check Run を確認する。通常の重複実行制御は repo + PR 単位で、同じ配信の再送を
永久に除外する仕組みではない。再配信すると API 利用が再度発生する可能性がある。

## 運用・バックアップ・ロールバック

- **永続キューではない**。Webhook の 202 後の hydration と待ち行列はメモリー内にある。
  強制停止では承認済みイベントが失われ得る。自動リリースを切り、実行・待機中のレビューが
  ない時間にデプロイする。強制停止後は GitHub の配信履歴から必要なイベントを選んで再配信する。
  永続化済みの `pending` / `running` が自動復旧する機能もないので、それらを確認して再実行する。
- **Render のディスクは 1 インスタンス専用**で、ディスクを持つサービスのデプロイ時には短い停止がある。
  配信失敗は GitHub の履歴で確認して再配信する。GitHub が自動で再送すると仮定しない。
- **監査 DB は増える**。PR diff、LLM メッセージ、ツール出力が保存され、削除の自動運用は未実装。
  初期 1 GB の使用量を監視し、容量拡張またはバックアップ後のデータ保持方針を決める。
  private リポジトリのコードを含み得るため、API token とバックアップのアクセスも制限する。
- **SQLite の稼働中バックアップ**は WAL ファイルを無視した単純コピーを避ける。
  イメージには sqlite3 CLI を含めている。サービスの Shell で次を実行し、生成物を
  暗号化した外部ストレージ等へ退避する（同じディスクに置いただけでは災害対策にならない）。

```sh
umask 077
mkdir -p /data/backups
sqlite3 /data/mochi.db ".backup '/data/backups/mochi-backup.db'"
sqlite3 /data/backups/mochi-backup.db 'PRAGMA integrity_check;'
```

バックアップには監査時点の一貫した DB が入る。Render のディスクスナップショットだけに
DB の復旧を依存させない。復元はサービス停止中に行い、元 DB と WAL/SHM を別途退避してから
復元 DB を配置し、所有者 10001:10001、`integrity_check`、過去 run の照会を確認する。
リリース前にもバックアップを取得し、コードを戻す際はマイグレーション互換性を確認する。
公開 API の破壊的変更や DB 変更がある場合、コードの Rollback だけで戻るとは限らない。

参考: [Render 永続ディスクの制約](https://render.com/docs/disks)、
[Blueprint 仕様](https://render.com/docs/blueprint-spec)、
[Railway](https://railway.com/pricing)。
