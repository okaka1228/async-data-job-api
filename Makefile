.PHONY: all build test lint up down migrate seed clean fmt

all: lint test build

build:
	go build -o bin/server ./cmd/server

run: build
	./bin/server

test:
	go test -v -race -count=1 ./...

test-integration:
	go test -v -race -count=1 -tags=integration ./...

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
