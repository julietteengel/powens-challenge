.PHONY: up down test

up:
	docker compose up --build

down:
	docker compose down

test:
	go test ./...
