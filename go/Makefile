APP     := jssc
BIN_DIR := bin
BIN     := $(BIN_DIR)/$(APP)

# The build is pure Go: fsst, the sole pool codec, needs no cgo.
GO      ?= go

.PHONY: all build install test race cover fuzz fuzz-open bench vet fmt lint tidy demo example clean

all: build

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/jssc

install:
	CGO_ENABLED=0 $(GO) install ./cmd/jssc

test:
	CGO_ENABLED=0 $(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

cover:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

# Round-trip fuzzing: valid JSON in, the same value out.
fuzz:
	$(GO) test -run xxx -fuzz FuzzRoundTrip -fuzztime $(or $(FUZZTIME),60s) ./jssc

# Decoder fuzzing: arbitrary bytes must fail cleanly, never panic or hang.
fuzz-open:
	$(GO) test -run xxx -fuzz FuzzOpen -fuzztime $(or $(FUZZTIME),60s) ./jssc

bench:
	$(GO) test -bench . -benchmem ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: fmt vet

tidy:
	$(GO) mod tidy

example:
	$(GO) run ./examples/basic

# End-to-end walkthrough of the command line over the bundled test data.
demo: build
	@./$(BIN) algorithms
	@echo
	@rm -rf $(BIN_DIR)/demo && mkdir -p $(BIN_DIR)/demo
	@cp jssc/testdata/users.json jssc/testdata/events.ndjson $(BIN_DIR)/demo/
	./$(BIN) compress $(BIN_DIR)/demo/users.json
	./$(BIN) compress --format ndjson $(BIN_DIR)/demo/events.ndjson
	@echo
	./$(BIN) info $(BIN_DIR)/demo/users.json.jssc
	@echo
	./$(BIN) query --stats '$$[*].name' $(BIN_DIR)/demo/users.json.jssc
	@echo
	./$(BIN) query --values '$$[*].actor.login' $(BIN_DIR)/demo/events.ndjson.jssc

clean:
	$(GO) clean
	rm -f coverage.out
	rm -rf $(BIN_DIR)
