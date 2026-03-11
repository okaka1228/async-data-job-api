# API Examples

## Create a job

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_url": "https://jsonplaceholder.typicode.com/posts",
    "idempotency_key": "import-posts-001",
    "max_retries": 5
  }'
```

Response (202 Accepted):
```json
{
  "id": "a1b2c3d4-...",
  "idempotency_key": "import-posts-001",
  "status": "pending",
  "input_url": "https://jsonplaceholder.typicode.com/posts",
  "total_rows": 0,
  "processed_rows": 0,
  "retries": 0,
  "max_retries": 5,
  "created_at": "2026-03-10T12:00:00Z",
  "updated_at": "2026-03-10T12:00:00Z"
}
```

## List jobs

```bash
# All jobs
curl http://localhost:8080/api/v1/jobs

# Filter by status
curl "http://localhost:8080/api/v1/jobs?status=failed&limit=10&offset=0"
```

## Get job details

```bash
curl http://localhost:8080/api/v1/jobs/{job-id}
```

## Cancel a job

```bash
curl -X POST http://localhost:8080/api/v1/jobs/{job-id}/cancel
```

## View DLQ failure entries

```bash
curl http://localhost:8080/api/v1/jobs/{job-id}/failures
```

## Health check

```bash
curl http://localhost:8080/healthz
```

## Prometheus metrics

```bash
curl http://localhost:8080/metrics
```

## Idempotency

同じ `idempotency_key` で再度リクエストすると、最初に作成されたジョブがそのまま返されます（200 OK）:

```bash
# 1回目: 202 Accepted — 新規作成
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"input_url": "https://example.com/data.json", "idempotency_key": "dedup-key-1"}'

# 2回目: 200 OK — 既存ジョブを返却
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"input_url": "https://example.com/data.json", "idempotency_key": "dedup-key-1"}'
```
