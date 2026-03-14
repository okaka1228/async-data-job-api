# ADR-001: Architecture Decisions

## Status: Accepted

## Context

大規模 JSON/NDJSON データの非同期処理基盤を構築する。API 受付 → キュー → ワーカー → 結果保存 のパイプラインが必要。

## Decisions

### HTTP Router: go-chi/chi
- 標準 `net/http` 互換でミドルウェア合成が容易
- 軽量で依存が少ない

### DB Access: plain SQL (database/sql + pgx driver)
- ORM のマジックを避け、SQL を直接制御
- パフォーマンスチューニングが容易

### Queue: Channel-based in-memory with Backup Polling
- 初期実装は単一プロセスで十分
- `Queue` インターフェースにより Redis 実装へ後から差し替え可能
- Redis は optional として将来拡張用に設計
- メモリ上のキュー上限（バッファ溢れ）やプロセスダウンによる未処理ジョブ（pending）消失に備え、定期的にDBを監視して再キューイングする Poller 機構を併用。

### Worker: Goroutine pool
- `sync.WaitGroup` による graceful shutdown
- 同時実行数を環境変数で制御
- context.WithTimeout でジョブ単位のタイムアウト

### Retry: Inline retry with DLQ + Manual retry + Webhook notification
- 失敗時にリトライカウントをインクリメントし pending に戻す
- `max_retries` 超過後は `failed` に遷移し `failed_job_entries` テーブルに記録（DLQ）
- 永続失敗時に `callback_url` へ Webhook を fire-and-forget で送信。`Notifier` インターフェース経由で将来的にキューバック再送へ差し替え可能
- `POST /api/v1/jobs/{id}/retry` で `failed` ジョブをユーザーが手動再実行可能。`retries=0` にリセットしてフルのリトライバジェットを付与

### Migration: golang-migrate
- Docker Compose との統合が容易（専用コンテナで実行）
- SQL ファイルベースで直感的

### Observability
- **Structured logging**: Go 1.21+ の `log/slog` (JSON handler)
- **Metrics**: Prometheus client_golang (promauto)
- **Tracing**: OpenTelemetry (デフォルトはstdout出力、環境変数 `OTEL_EXPORTER_OTLP_ENDPOINT` で Jaeger などの OTLP エンドポイントへ動的にルーティング可能)

### Idempotency
- `idempotency_key` カラムに UNIQUE 制約
- 同一キーでの再リクエストは既存ジョブを返却（200 OK）

## Consequences

- 単一プロセス限定（スケールアウトには Redis queue 実装が必要）
- DLQ は DB テーブルベースで簡素（専用の DLQ サービスではない）
- Tracing は環境変数一つで OTLP (HTTP経由) へ転送可能になったため、本番環境への監視ツールの後付けが容易
