.PHONY: all build test test-integration test-coverage lint up down migrate seed clean fmt

all: lint test build

build:
	go build -o bin/server ./cmd/server

run: build
	./bin/server

test:
	go test -v -race -count=1 ./...

test-integration:
	go test -v -race -count=1 -tags=integration ./...

test-coverage:
	go test -coverprofile=coverage.out -tags=integration ./...
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report saved to coverage.html"

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .
	goimports -w .

up:
	docker compose up --build -d

up-seed:
	docker compose --profile seed up --build -d

down:
	docker compose --profile seed down -v

migrate:
	docker compose up migrate

logs:
	docker compose logs -f app

clean:
	rm -rf bin/
	docker compose down -v --remove-orphans
