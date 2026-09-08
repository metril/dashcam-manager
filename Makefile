.PHONY: build run test docker-build up down frontend

build:
	go build -o dashcam-api ./cmd/api

run: build
	./dashcam-api

test:
	go vet ./...
	go test ./...

frontend:
	cd app/frontend && npm run build

docker-build:
	docker compose build

up:
	docker compose up -d

down:
	docker compose down
