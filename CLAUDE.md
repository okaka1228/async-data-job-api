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
- **Lint**: golangci-lint

## よく使うコマンド

```bash
# ビルド
make build

# ユニットテスト（race detector 付き）
go test -v -race -count=1 ./...

# 統合テスト（PostgreSQL 必要）
go test -v -race -count=1 -tags=integration ./...

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

## ディレクトリ構成

```
cmd/server/          エントリポイント（main.go）
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
- **冪等性**: `idempotency_key` (UNIQUE) で重複作成を防止、既存ジョブを返す

## テスト方針

- **ユニットテスト**: `_test.go` ファイル、モックリポジトリ使用、`go test ./...`
- **統合テスト**: `//go:build integration` タグ、実 PostgreSQL 接続、`go test -tags=integration ./...`
- **Prometheus テスト注意**: `promauto` はグローバル登録のため、テスト内では `NewMetrics()` をパッケージレベルで一度だけ呼ぶ

## API エンドポイント

| Method | Path                        | 概要             |
|--------|-----------------------------|------------------|
| POST   | `/api/v1/jobs`              | ジョブ作成       |
| GET    | `/api/v1/jobs`              | ジョブ一覧       |
| GET    | `/api/v1/jobs/{id}`         | ジョブ詳細       |
| POST   | `/api/v1/jobs/{id}/cancel`  | ジョブキャンセル   |
| GET    | `/api/v1/jobs/{id}/failures`| DLQ 失敗履歴     |
| GET    | `/healthz`                  | ヘルスチェック     |
| GET    | `/metrics`                  | Prometheus       |

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
