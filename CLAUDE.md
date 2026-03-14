# CLAUDE.md

このファイルは Claude Code にプロジェクトのコンテキストを提供するためのものです。

## プロジェクト概要

大規模 JSON/NDJSON データを非同期処理するジョブ基盤 API。  
ファイル URL を受け取り → ジョブ登録 → バックグラウンドワーカーがストリーム処理 → 結果保存。

## 技術スタック

- **Language**: Go 1.22
- **HTTP Router**: go-chi/chi v5
- **DB**: PostgreSQL 16 (plain SQL, `database/sql` + `pgx` ドライバ)
- **Migration**: golang-migrate
- **Queue**: Channel ベースのインメモリキュー（`Queue` インターフェースで Redis 差替え可能）
- **Worker**: goroutine pool（自前実装）
- **Observability**: OpenTelemetry (tracing) + Prometheus (metrics) + slog (structured logging)
- **Container**: Docker Compose
- **CI**: GitHub Actions
- **Lint**: golangci-lint v2

## よく使うコマンド

```bash
# ビルド
make build

# ユニットテスト（race detector 付き）
go test -v -race -count=1 ./...

# 統合テスト（PostgreSQL 必要）
go test -v -race -count=1 -tags=integration ./...

# テストカバレッジの出力 (coverage.out / coverage.html)
make test-coverage

# lint
make lint

# Docker Compose 起動
make up

# シードデータ付き起動
make up-seed

# 停止 & ボリューム削除
make down

# ログ表示
make logs
```

## Push 前チェック

```bash
# lint + test を必ず通してからpushする
make lint
go test -timeout 30s ./internal/...
```

## テストカバレッジ

主要パッケージの関数カバレッジ（`cmd/` と DB 接続の `repository/` 除く）。

- `internal/config`, `queue` → **100%**
- `internal/domain` → **80%**
- `internal/api` ハンドラ群 → **73〜100%**（全ハンドラにユニットテストあり）
- `internal/worker` processor/notifier → **75〜93%**
- `repository/` は統合テスト（`-tags=integration`）でカバー

## ディレクトリ構成

```
cmd/
  server/            サーバーエントリポイント（main.go）
  seed/              開発用シードデータ投入（main.go）。本番バイナリとは分離
internal/
  api/               HTTP ハンドラ、ルーター、ミドルウェア
  config/            環境変数からの設定読み込み
  domain/            ドメインモデル、バリデーション、定数
  observability/     Prometheus メトリクス、OpenTelemetry トレーサー
  queue/             Queue インターフェース + Channel 実装
  repository/        JobRepository インターフェース + plain SQL 実装
  worker/            WorkerPool + Processor（リトライ、タイムアウト、DLQ）
migrations/          SQL マイグレーションファイル
docs/                ADR、API examples、OpenAPI spec
```

## アーキテクチャ方針

- **ORM を使わない**: `database/sql` + plain SQL で制御を明確にする
- **インターフェース分離**: `Queue`, `JobRepository` はインターフェースで定義し、テスト時はモック差替え
- **ジョブ状態遷移**: `pending` → `running` → `succeeded` / `failed` / `canceled`
- **リトライ**: 失敗時に `retries` をインクリメントし `pending` に戻す。`max_retries` 超過で `failed`
- **DLQ**: 失敗履歴は `failed_job_entries` テーブルに記録
- **Webhook 通知**: 永続失敗時に `callback_url` へ fire-and-forget で HTTP POST。`Notifier` インターフェース経由で将来的なキューバック再送に差し替え可能
- **手動リトライ**: `POST /api/v1/jobs/{id}/retry` で `failed` ジョブを `pending` に戻す（`retries=0` リセット）。`RetryJob` が `UPDATE WHERE status='failed' RETURNING *` で atomic に実行
- **冪等性**: `idempotency_key` (UNIQUE) で重複作成を防止、既存ジョブを返す
- **環境変数は Config で一元管理**: `OTEL_EXPORTER_OTLP_ENDPOINT` を含む全変数を `internal/config/config.go` の `Config` 構造体で管理し、`os.Getenv` の直接呼び出しは行わない
- **キャンセル/リトライは 1 クエリで完結**: `CancelJob` / `RetryJob` が `UPDATE ... WHERE status IN (...) RETURNING *` で状態確認と更新を atomic に実行。失敗時のみ `GetByID` で 404/409 を判別する

## テスト方針

- **ユニットテスト**: `_test.go` ファイル、モックリポジトリ使用、`go test ./...`
- **統合テスト**: `//go:build integration` タグ、実 PostgreSQL 接続、`go test -tags=integration ./...`
- **Prometheus テスト注意**: `promauto` はグローバル登録のため、テスト内では `NewMetrics()` をパッケージレベルで一度だけ呼ぶ
- **ベンチマーク**: `internal/worker/processor_bench_test.go` に 1GB 処理ベンチマークあり（後述）

## API エンドポイント

| Method | Path                        | 概要             |
|--------|-----------------------------|------------------|
| POST   | `/api/v1/jobs`              | ジョブ作成       |
| GET    | `/api/v1/jobs`              | ジョブ一覧       |
| GET    | `/api/v1/jobs/{id}`         | ジョブ詳細       |
| POST   | `/api/v1/jobs/{id}/cancel`  | ジョブキャンセル   |
| POST   | `/api/v1/jobs/{id}/retry`   | 失敗ジョブの再実行 |
| GET    | `/api/v1/jobs/{id}/failures`| DLQ 失敗履歴     |
| GET    | `/healthz`                  | ヘルスチェック     |
| GET    | `/metrics`                  | Prometheus       |

## パフォーマンス特性

Intel Core Ultra 7 265 (4コア) / Go 1.22 での計測値。

### 1GB ファイル処理時間（HTTP ストリーム）

| フォーマット | 処理時間 | スループット | 行数 |
|------------|---------|------------|------|
| NDJSON     | 約 2.3 秒 | 475 MB/s   | ~14M |
| JSON array | 約 4.5 秒 | 236 MB/s   | ~14M |

JSON array が NDJSON の約 2 倍遅い主因は **フォーマットの構造差**（約 81%）。NDJSON は `\n` の1バイト比較で要素境界を検出できるのに対し、JSON array は文字列・エスケープ・括弧深さを追跡するステートマシンが必要なため、境界検出コスト自体が高い。残り約 19% は `json.Decoder` が要素を二重スキャンする実装上のオーバーヘッド（カスタム SM + `json.Valid()` に替えると 233→279 MB/s だが NDJSON には届かない）。

### API スループット（50並列）

| エンドポイント | RPS | p50 | p99 |
|--------------|-----|-----|-----|
| `GET /healthz` | ~53,400 | 0.8ms | 3.6ms |
| `GET /api/v1/jobs` | ~5,900 | 7.1ms | 32.6ms |
| `GET /api/v1/jobs/{id}` | ~20,900 | 2.0ms | 7.5ms |
| `POST /api/v1/jobs` | ~10,600 | 4.1ms | 13.3ms |

### 主な最適化ポイント

- **スキャナーバッファ再利用**: `sync.Pool` で 1MB バッファをワーカー間共有
- **コンテキストチェック間引き**: 全行ではなく 1000 行ごとに `ctx.Done()` 確認
- **複合インデックス**: `(status, updated_at) WHERE status='pending'` で poller クエリを高速化（migration 000002）
- **スライス事前確保**: `List`, `ListFailedEntries`, `FetchPendingJobs` で `make([]T, 0, limit)` を使用
- **DB 接続プール**: `MaxOpenConns: 25` / `MaxIdleConns: 25` で高負荷時の接続再確立コストを削減
- **HTTP Transport 設定**: `MaxIdleConns: 100` / `MaxIdleConnsPerHost: 16` でコネクション再利用を最適化

## 環境変数

| 変数名               | デフォルト値 | 説明 |
|----------------------|-------------|------|
| `DATABASE_URL`       | `postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable` | DB 接続文字列 |
| `SERVER_PORT`        | `8080`      | HTTP ポート |
| `WORKER_CONCURRENCY` | `4`         | ワーカー同時実行数 |
| `WORKER_TIMEOUT_SEC` | `300`       | ジョブタイムアウト（秒） |
| `WORKER_MAX_RETRIES` | `3`         | 最大リトライ回数 |
| `LOG_LEVEL`          | `info`      | ログレベル |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (空)      | OTLPトレース送信先（例:`http://jaeger:4318`）。空の場合は標準出力 |
