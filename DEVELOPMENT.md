# Development Guide

このガイドは `async-data-job-api` の開発者向けドキュメントです。開発環境の構築からテスト、運用コマンド、新しい機能の追加方法について説明します。

## 前提条件

- [Go](https://go.dev/doc/install) 1.22 以上
- [Docker](https://docs.docker.com/get-docker/) & [Docker Compose](https://docs.docker.com/compose/install/)
- `make` コマンド
- [golangci-lint](https://golangci-lint.run/docs/install/) v2（CI で使用）

---

## 開発環境のセットアップ

### 1. リポジトリのクローンと依存解決

```bash
git clone <repository_url>
cd async-data-job-api
go mod tidy
```

### 2. ローカルインフラの起動

開発用の PostgreSQL とマイグレーションツールを起動します。

```bash
# 全てのサービス（DB, API, Prometheus）を起動する場合
make up

# DBだけを起動し、APIはローカル（ホスト側）でデバッグ実行する場合
docker compose up -d postgres
```

### 3. アプリケーションのローカルデバッグ実行

コンテナではなく、手元のターミナルで実行するには以下のコマンドを使用します。
事前に `docker compose up -d postgres` でDBを起動し、マイグレーションを実行しておく必要があります。

```bash
# マイグレーション（初回のみ）
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
migrate -path ./migrations -database "postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable" up

# 環境変数を指定して実行
DATABASE_URL="postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable" make run
```

---

## テストの実行

テストは「依存関係なしで動くUnit Test」と「実DBを必要とするIntegration Test」に分かれています。
現在のテストカバレッジは **84.6%** です（`cmd/` と `repository/` DB接続部分を除く）。

### Unit Test (DB不要)

ドメインロジック、キュー、モックを利用したAPIハンドラのテストを実行します。

```bash
make test
# または
go test -v -race -count=1 ./...
```

### Integration Test (DB必須)

Repository 層の実DBへのクエリテストを実行します。実行前に `postgres` コンテナを起動してください。

```bash
# 1. DBコンテナの起動
docker compose up -d postgres
# 2. マイグレーション実行
migrate -path ./migrations -database "postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable" up
# 3. 統合テスト実行 (-tags=integration が必要)
make test-integration
```

### カバレッジレポート

```bash
# カバレッジレポートを生成（coverage.html）
make test-coverage
```

---

## Lint とフォーマット

**コミット・Push前に必ず Lint とテストを通してください。**

```bash
# Push前チェック（必須）
make lint
go test -timeout 30s ./internal/...

# gofmt の実行
make fmt
```

> **Note**: `make fmt` は `goimports` も使用します。未インストールの場合は `go install golang.org/x/tools/cmd/goimports@latest` を実行してください。

> **Note**: golangci-lint v2 を使用しています。設定は `.golangci.yml`（`version: "2"` 形式）を参照してください。

---

## DBマイグレーション

DBスキーマの変更は `golang-migrate` を使用します。

### 新しいマイグレーションファイルの作成

`migrations` ディレクトリに直接SQLファイルを作成するか、`migrate create` コマンドを使用してください。

```bash
migrate create -ext sql -dir migrations -seq create_users_table
```

### マイグレーションの適用

```bash
# ローカルDBに対して適用
migrate -path ./migrations -database "postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable" up
```

※ `docker compose up` を実行した場合は `migrate` コンテナが自動で `up` 処理を行います。

---

## Git ワークフロー

- **main ブランチへの直接 push は禁止です。** 必ず別ブランチを切って PR を作成してください。
- ドキュメントのみの変更（`*.md`, `LICENSE`, `docs/**`）では CI が実行されません。

---

## アプリケーションの拡張・改修ガイド

### 1. APIエンドポイントを追加する場合
1. `internal/api/handler_xxx.go` にハンドラを作成する
2. `internal/api/router.go` のルーティング設定に追加する
3. 必要なリクエスト・レスポンスの構造体を定義する
4. `docs/openapi.yml` (OpenAPI仕様書) を更新する

### 2. 新しいWorkerロジック（ビジネスロジック）を追加する場合
現在、巨大データのストリーミング読み込みとDBへの小まめな進捗保存（基盤機能）は実装済みです。
データの中身に応じた集計・別サービスへの送信などを行いたい場合は、以下の手順で拡張してください。

1. **ドメインモデルの追加**: 読み込んだ1行のデータをデコードする為の構造体を定義する。
2. **Processorの書き換え**: `internal/worker/processor.go` 内の `processNDJSON` または `processJSON` で、`json.Valid(line)` による検証を行っている部分を `json.Unmarshal` に差し替え、ビジネスロジックを呼ぶように変更する。
3. **リポジトリの拡張**: DB保存が必要な場合は `internal/repository` に処理を追加して登録する。
