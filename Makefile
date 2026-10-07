.DEFAULT_GOAL := help

.PHONY: help check frontend-build build preview dev web-dev

help:
	@echo 'check          Go tests, vet, and frontend type check/build (run npm --prefix web ci first)'
	@echo 'build          Build web/dist and bin/paper-digest'
	@echo 'preview        Render offline Markdown fixture'
	@echo 'dev            Start Go with config/config.local.json (see docs/development.md)'
	@echo 'web-dev        Start Vite at 127.0.0.1:5173'

check:
	go test ./...
	go vet ./...
	$(MAKE) frontend-build

frontend-build:
	npm --prefix web run build

build: frontend-build
	mkdir -p bin
	go build -o bin/paper-digest ./cmd/paper-digest

preview:
	go run ./cmd/paper-digest preview ./testdata/preview.json

dev:
	go run ./cmd/paper-digest --config config/config.local.json serve

web-dev:
	npm --prefix web run dev
