# 4 専門エージェントへの移行

動作済みの small プリセットから、4 専門エージェント + 統合役 1 つへ移行する。
Render のプラン、ディスク、既存の秘密情報はそのまま使う。
稼働中・待機中のレビューがない時間に切り替える。

| 担当 | 実行環境 |
| --- | --- |
| correctness | 専用 Daytona Sandbox 1 台 |
| security | 専用 Daytona Sandbox 1 台 |
| performance-architecture | 専用 Daytona Sandbox 1 台 |
| test-runner | 専用 Daytona Sandbox 1 台 |
| synthesizer（結果の統合） | Render 上で Gemini を呼び出す。Sandbox は不要 |

専門エージェントは同じ PR の head SHA をそれぞれ独立してチェックアウトする。
`fork_from_base: false` のため、ベース VM の作成や fork は行わない。
全員 `gemini-3.8-flash` を使い、登録済みの `GEMINI_API_KEY` を共有する。
追加の OpenAI / Anthropic キーや GitHub App の権限変更は不要。

## 1. 変更をマージする

PR の GitHub Actions CI が成功してから `main` にマージする。
CI は通常の Go 検査に加え、次を確認する。

- 両 workflow が Gemini のキーだけで runtime validation を通ること。
- fork 非対応環境で 4 専門エージェントが異なる Sandbox ID を使うこと。
- 成功時と専門エージェントの失敗時に Sandbox を削除すること。
- synthesizer が Sandbox を作成しないこと。
- Go 用 Snapshot の Dockerfile がビルドでき、Go、git、ripgrep が使えること。

これらは実際の Daytona / Gemini 接続を検証しない。実接続は手順 4 で確認する。
Render の自動デプロイは無効のため、マージだけでは本番は切り替わらない。

## 2. 新しい Daytona Snapshot を作る

Daytona CLI が使える手元の端末で、マージ後のリポジトリを開いて実行する。
`git pull --ff-only` は作業中の変更を片付け、`main` に切り替えてから行う。

```sh
git switch main
git pull --ff-only
daytona login
daytona snapshot create mochi-review-go \
  --dockerfile deploy/sandbox/go.Dockerfile \
  --cpu 1 --memory 2
daytona snapshot list
```

Snapshot が利用可能な状態になるまで待つ。名前は **`mochi-review-go`** とする。
Go 1.27.4 とレビュー用の基本ツールを含む通常のコンテナ型 Snapshot でよい。
稼働中の small プリセットが使う **`mochi-review` は残す**。
サーバー用のルート `Dockerfile` は指定しない。

CPU 1 / メモリー 2 GB は初回検証の出発点で、対象リポジトリによって調整する。
アカウントの同時 Sandbox 上限が少なくとも 4 台、リソース上限が合計 CPU 4 / メモリー 8 GB
を許容することを確認する。他の Sandbox を利用中ならその分の余裕も必要。
Snapshot の保存と Sandbox の利用には Daytona 側の料金が発生し得る。
API キーを Snapshot に含めない。

Go 以外のリポジトリには必要な言語ランタイムを追加した別 Snapshot を用意する。
このイメージだけであらゆる言語のテストを実行できるわけではない。

参考: [Daytona Snapshot 作成](https://www.daytona.io/docs/en/snapshots.md#snapshots-from-local-images)。

## 3. Render の使用 workflow を切り替える

Snapshot の準備完了後に、既存の `mochi-qvzc` サービスの Environment で次の **1 項目**を変更する。

```text
MOCHI_WORKFLOW=configs/workflows/pr-review.yaml
```

既存の API キーや GitHub App の秘密情報は再登録しない。
`MOCHI_WORKERS=1`、`MOCHI_QUEUE_SIZE=16` を維持する。
workers は同時 workflow 数であり、専門エージェント数ではない。

変更を保存し、マージ後の最新 `main` を **1 回**デプロイする。
保存時に rebuild / deploy した場合、重ねて Manual Deploy しない。
デプロイ対象が今回のマージを含むこと、状態が `live` になること、
`https://mochi-qvzc.onrender.com/readyz` が 200 になることを確認する。

既存サービスの環境変数は Dockerfile の既定値より優先されるため、コードの更新だけでは
small から切り替わらない。`render.yaml` の値も full に揃えてあるが、この 1 項目の更新のために
別のサービス作成や未確認の Blueprint 計画を適用する必要はない。
`validate --runtime` と `/readyz` は Snapshot の存在や外部 API の利用可否を確認しない。

## 4. 小さな Go の PR で実行する

最初は Mochi 自身などの Go リポジトリで、小さな non-draft PR を更新して自動レビューを起動する。
Issue comment を購読していれば、所有者などが `/mochi review` とコメントしてもよい。
1 回の実行について Check Run、Render ログ、run の監査記録、Daytona を確認する。

- 4 専門エージェントと synthesizer の結果が揃い、失敗した担当がないこと。
- 4 台が同じ PR の head SHA を使い、Sandbox ID はそれぞれ異なること。
- synthesizer に Sandbox ID が付かないこと。
- test-runner が実際にテストを実行した場合、実行コマンドと終了結果が `test_execution` に残ること。
  未実行の場合は `skipped_reasons` を確認し、テスト成功と扱わない。
- レビュー完了後に 4 台が削除されること。失敗時も残存 Sandbox を確認すること。
- Gemini のレート制限、Daytona の同時実行制限、Render のメモリー不足がないこと。

初回は依存パッケージの取得も必要になる。Go の大きなリポジトリでテストが時間切れに
なる場合は、実行範囲や Snapshot の依存キャッシュ、テスト担当のリソースを調整する。
統合結果が投稿されただけで、全担当の成功やテスト実行完了と判断しない。

Render はまず Starter のままで計測する。Daytona / Gemini の使用量、所要時間、
Render の最大メモリーを数件で確認し、必要な項目だけ調整する。
4 担当を並列化しても費用や所要時間が厳密に 4 倍になるわけではない。
workflow のトークン予算は呼び出し後の集計による停止条件で、厳密な金額上限ではない。
API 側の予算通知も併用する。

## 切り戻し

問題があれば、実行中・待機中のレビューがない時間に Render の Environment を戻して保存・デプロイする。

```text
MOCHI_WORKFLOW=configs/workflows/pr-review-small.yaml
```

small の YAML と既存の `mochi-review` Snapshot は変更しないため、そのまま利用できる。
新しい `mochi-review-go` Snapshot は削除不要だが、残存 Sandbox は停止・削除を確認する。
再デプロイ時の停止、メモリー内キュー、監査 DB の扱いは [運用手順](deployment.md#運用バックアップロールバック) を参照する。
