# Makefile for VerbooRTC development and testing infrastructure.
#
# This file provides convenience targets to spin up and tear down local infra
# (NATS cluster, Redis single instance or cluster), generate gRPC code from .proto,
# create TLS certificates for local secure transports, and run tests/benchmarks.
#
# Targets overview:
#   tests             : Run the full Go test suite with race detector enabled.
#   test-bench        : Run all Go benchmarks (with -tags=bench) to measure performance.
#
# Notes:
#   - All recipe lines must be indented with a literal TAB.
#   - Adjust COMPOSE_FILE if using a different docker-compose config in CI.
#   - To fix accidental spaces in indentation: sed -i -E 's/^( {4})+/\t/' Makefile


# Timeouts for wait loops (if you add readiness checks)
WAIT_TIMEOUT := 60
WAIT_INTERVAL := 1

# Protobuf source and output directories
PROTO_DIR := protos
OUT_DIR := protos/gen

.PHONY: tests test-bench gen-proto tls build-client-sdk

# Generate Go message types and gRPC client/server code from signaling.proto.
# Produces signaling.pb.go (message types) and signaling_grpc.pb.go (gRPC service interfaces and client stubs).
gen-proto:
	@ export GOPATH=$HOME/go;
	export PATH=$PATH:$GOPATH/bin;
	mkdir -p $(OUT_DIR)
	protoc \
	  --proto_path=$(PROTO_DIR) \
	  --go_out=$(OUT_DIR) --go_opt=paths=source_relative \
	  --go-grpc_out=$(OUT_DIR) --go-grpc_opt=paths=source_relative \
	  $(PROTO_DIR)/signaling.proto

# Generate a self-signed TLS certificate and key for local development.
# These are used by QUIC, gRPC and WebSocket transports in secure mode.
tls:
	mkdir -p tls
	openssl req -x509 -newkey rsa:2048 -nodes \
	  -keyout tls/tls.key -out tls/tls.crt -days 365 \
	  -subj "/CN=verboo-rtc" \
	  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"

# Run the full Go test suite with race detector enabled.
# Does not automatically start infra; ensure NATS/Redis are up before running.
tests:
	go test -race ./... -v -count=1

# Run all Go benchmarks in the repository (with -tags=bench).
# Benchmarks are run without race detector for performance accuracy.
test-bench:
	@echo "==> running go benchmarks (no -race) ..."
	@go test -tags=bench ./... -bench="." -benchmem -run=^$$

build-client-sdk:
	@echo "==> building client SDK..."
	@go build -o verboo-sdk-chat-client ./example/chat-client
