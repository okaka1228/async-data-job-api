# Architecture Document

このドキュメントは `async-data-job-api` のシステムアーキテクチャ、データモデル、およびコンポーネント設計について解説します。技術的な意思決定の背景は `docs/adr/001-architecture.md` も併せて参照してください。

---

## 1. システム全体図

本システムは単一の Go プロセス内で完結する非同期ジョブ基盤です。  
コンポーネントは大きく分けて「API」「データベース」「キュー」「ワーカー」から構成されます。

```text
+-----------------------+           +-----------------------+
|      Client           |           |   External HTTP API   |
| (Web UI / Server)     |           |   (Data Source)       |
+-----------+-----------+           +-----------+-----------+
            | HTTP POST                         ^
            v                                   | HTTP GET (Stream)
+-----------------------+                       |
|   async-data-job-api  |                       |
|                       |                       |
| +-------------------+ |           +-------------------+
| |    API Router     | |           | Worker Pool       |
| | (go-chi/chi v5)   +-|--Enqueue->| (Goroutines)      |
| +---------+---------+ |           +---------+---------+
|           |           |                     |
|           | DB Access |           DB Access |
|           v           |                     v
| +-------------------+ |           +-------------------+
| |                   | |           |                   |
| |  JobRepository    +-|-----------|  JobRepository    |
| |  (plain SQL)      | |           |  (plain SQL)      |
| |                   | |           |                   |
| +---------^---------+ |           +-------------------+
|           |           |
| +---------+---------+ |
| | Background Poller | |
| | (Pending Jobs)    |-|--Enqueue (Recovery)
| +-------------------+ |
+-----------+-----------+
            |
            v
+-----------------------+
|  PostgreSQL 16        |
|  (jobs, failed_job)   |
+-----------------------+
```

---

## 2. コアコンポーネントの設計

### 2.1 API Layer (Router & Middleware)
- **依存ライブラリ**: `github.com/go-chi/chi/v5`
- ルーティング定義だけでなく、ロギング（`slog`）、リクエストID生成、メトリクス収集（Prometheus）、パニックリカバリーなどを `internal/api/middleware.go` のチェーンで一元化しています。
- **責務**: リクエストのバリデーション、冪等性キーによる既存ジョブチェック、DBとQueueへの「登録（Enqueue）」に特化し、重い処理は一切行いません。

### 2.2 Database Layer (Repository)
- **依存**: `database/sql`, `pgx/v5/stdlib`
- ORM を使用せずプレーンな SQL を直書きしています。進捗状況（`processed_rows`）の数千〜数万回に及ぶ小まめな更新（UPDATE）を無駄なく実行するためです。
- さらに I/O 競合を防ぐため、ワーカー側の進捗 DB 更新（`UpdateProgress`）は最大でも1秒に1回へスロットリングされています。
- `JobRepository` インターフェースで抽象化しており、単体テストではメモリ上の Mock へ差し替えられるようにしています。

### 2.3 Queue & Worker Layer
- **インメモリキュー**: 初期実装として `queue.ChannelQueue`（Go SDK のバッファ付きチャネル、デフォルト制限1000）を採用しています。API レイヤーから Job の UUID 文字列のみを投入します。
- **Worker Pool**: `internal/worker/pool.go` にて、環境変数 `WORKER_CONCURRENCY` で指定された数（デフォルト: 4）の goroutine が起動した状態で待機（Dequeue）しています。
- **Background Poller**: 突発的なスパイクアクセスによるキューのバッファ溢れやサーバー再起動によってインメモリから消失した `pending` ジョブを復旧（リカバリ）するため、10秒に一度DBを監視し、未処理ジョブをキューへ再投入する仕組み（`Poller`）を併設しています。
- **Graceful Shutdown**: プロセスに終了シグナル（SIGINT/TERM）が飛んだ際、`sync.WaitGroup` により実行中のジョブが完了・またはタイムアウトするまでHTTPサーバーを落とさずに終了を待機します。

### 2.4 Streaming Processor
- **ストリーム処理**: 指定された `input_url` から数 GB のデータをダウンロードする場合でも、`body.Bytes()` 等で全展開せずに `bufio.Scanner` または `json.Decoder` による **行単位または要素単位の逐次読み込み** を行っています。
- **エラーとリトライ**: 読み込み中のエラーや外部ネットワークタイムアウト等の障害に備え、エラー内容を `failed_job_entries` テーブルに記録（DLQ: Dead Letter Queue 機能）し、最大リトライ回数を超えなければステータスを再度 `pending` へ戻します。

---

## 3. データモデル（PostgreSQL スキーマ）

### 3.1 `jobs` テーブル
メインのジョブ情報を管理します。

| カラム名 | 型 | 説明 |
|---|---|---|
| `id` | UUID (PK) | ジョブの一意識別子 |
| `idempotency_key` | VARCHAR (UNIQUE) | 冪等性保証用のキー（同一リクエスト重複排除） |
| `status` | VARCHAR | `pending`, `running`, `succeeded`, `failed`, `canceled` |
| `input_url` | TEXT | 処理対象のデータ元URL |
| `processed_rows` | BIGINT | 正常に検証できた行数 |
| `retries` / `max_retries` | INT / INT | リトライ実行回数と上限回数 |
| `error_message` | TEXT | 最終的なエラー時のメッセージ（succeededならNULL） |

### 3.2 `failed_job_entries` テーブル
ジョブ実行失敗時の詳細ログを永続化するDLQ（Dead Letter Queue）の役割を果たします。ジョブ自体がリトライして成功しても、過去の失敗履歴は残ります。

| カラム名 | 型 | 説明 |
|---|---|---|
| `id` | UUID (PK) | 失敗レコードのID |
| `job_id` | UUID (FK) | 対象のジョブID（jobsテーブルと外部キー制約） |
| `error_message` | TEXT | 失敗時の詳細なエラーメッセージ |
| `attempt` | INT | 何回目のトライで失敗したか |

---

## 4.  observability (可観測性) の設計

本番環境でジョブの詰まりや負荷を監視するため、標準で可観測性基盤を組み込んでいます。

1. **Structured Logging (slog)**
   - APIリクエスト、ワーカーの起動・停止、ジョブのエラーは全てJSON形式の構造化ログとして出力されます。これにより、DatadogやCloudWatch等での抽出・集計が容易になります。
2. **Metrics (Prometheus)**
   - HTTPのレスポンス速度・コードなどの Web レイヤーのメトリクス。
   - `jobs_created_total`, `jobs_completed_total`, `jobs_failed_total` など、ジョブの成功/失敗レートを算定できるカスタムカウンタ。
3. **Tracing (OpenTelemetry)**
   - ジョブごとの処理フェーズ（Fetch, Parse など）の所要時間を追跡するため、OTel対応のトレーサーを初期化しています（現在は開発用に標準出力エクスポーターを設定）。

---

## 5. テスト戦略

- **ユニットテスト**: モックリポジトリを使用し、API ハンドラー・ワーカープロセッサ・キュー・ドメインロジックを網羅的にテスト（カバレッジ **84.5%**）
- **統合テスト**: `//go:build integration` タグで分離し、実 PostgreSQL に対してリポジトリ層のCRUD操作を検証
- **CI**: GitHub Actions で変更種別を自動検出し、ソース変更時は lint（golangci-lint）→ test（unit + integration）→ build の順で実行。`ci-gate` ジョブが単一の必須ステータスチェックとして機能し、ドキュメントのみの変更は即座に通過する
- **注意**: `FetchPendingJobs` には5分間のグレースピリオドフィルタを設けており、新規作成直後のジョブがポーラーに重複取得されないようにしています

---

## 6. 将来の拡張性（Future Extensibility）

このアーキテクチャはスケールアウトを前提とした拡張（Redis化）が簡単にできるよう設計されています。

### 複数サーバー（Pod）によるスケールアウトの実装プラン
現在の `ChannelQueue` を `Redis` や `RabbitMQ` などに置き換えることで、APIコンテナとWorkerコンテナを分離し、巨大な負荷に対してWorkerのコンテナの数だけを増やすことが可能です。

1. `queue.Queue` インターフェースを満たす `RedisQueue` を作成する（ex: Redisの `RPUSH` / `BLPOP` コマンドを使用）。
2. `cmd/server/main.go` にて、環境変数を判定して `ChannelQueue` から `RedisQueue` へ切り替える。
3. ポーリングまたは Pub/Sub 機能により、複数台の Worker が 1 つの Queue を安全に奪い合う構成とする。

---

> **Disclaimer**: 本プロジェクトの設計および実装は、AI（LLM）によって生成および支援されています。ご運用の際は必要に応じて適切なレビューを行ってください。
