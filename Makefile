.PHONY: up down test

up: .env
	docker compose up --build

.env:
	cp .env.example .env

down:
	docker compose down

test:
	go test ./...
