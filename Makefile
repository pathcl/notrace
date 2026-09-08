BINARY      := notrace
TEMPO_URL   ?= http://localhost:3200
LAB_DIR     := lab

.PHONY: build test test-integration test-all lab-up lab-down lint

build:
	go build -o $(BINARY) .

test:
	go test ./...

lint:
	go vet ./...

test-integration:
	NOTRACE_TEMPO_URL=$(TEMPO_URL) go test -tags integration -v -timeout 60s ./integration/...

test-all: lab-up test
	$(MAKE) test-integration
	$(MAKE) lab-down

lab-up:
	docker compose -f $(LAB_DIR)/docker-compose.yaml up -d --build --wait

lab-down:
	docker compose -f $(LAB_DIR)/docker-compose.yaml down -v

lab-logs:
	docker compose -f $(LAB_DIR)/docker-compose.yaml logs -f

# Quick smoke test — search last 2 minutes and tail for 10s
smoke: build
	NOTRACE_TEMPO_URL=$(TEMPO_URL) ./$(BINARY) tempo search --start 2m
	NOTRACE_TEMPO_URL=$(TEMPO_URL) timeout 10 ./$(BINARY) tempo tail || true
