.PHONY: all server cli workers egress-template proto test test-integration test-conformance test-transport test-multinode test-media test-network test-sip test-sdks dev-deps lint clean load-samples

GO_PACKAGES := ./server/... ./api/... ./database/... ./pkg/... ./tests/load/... ./tests/interoperability/probe/... ./tests/sdk/... ./egress/... ./ingress/... ./sip/... ./sdks/... ./cli/...
TEST_DB ?= postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable
TEST_REDIS ?= redis://127.0.0.1:6380/0
PYTHON ?= python3

all: server

server:
	go build -o bin/streamkit-server ./server/cmd/streamkit-server

cli:
	go build -o bin/streamkit ./cli

workers:
	go build -o bin/streamkit-egress ./egress/cmd/streamkit-egress
	go build -o bin/streamkit-ingress ./ingress/cmd/streamkit-ingress
	go build -o bin/streamkit-sip ./sip/cmd/streamkit-sip

# After changing egress/template/src or sdk-js. The bundle is committed, so building the worker needs no Node.
egress-template:
	cd egress/template && npm ci && node build.mjs

proto:
	cd proto && buf generate

dev-deps:
	docker compose up -d --wait

test:
	go test -race $(GO_PACKAGES)

test-integration: dev-deps
	STREAMKIT_TEST_DATABASE_URL='$(TEST_DB)' STREAMKIT_TEST_REDIS_URL='$(TEST_REDIS)' go test -race -count=1 $(GO_PACKAGES)

test-conformance: server
	cd tests/interoperability && npm ci && npm run bundle && node --test --test-reporter=spec conformance.test.mjs

# Codec negotiation, A/V sync, SVC, data tracks and /rtc/v1, on this server and the reference. See tests/interoperability/lib/targets.mjs.
test-transport: server
	go build -o bin/transport-probe ./tests/interoperability/probe
	cd tests/interoperability && npm ci && npm run bundle && node --test --test-concurrency=1 --test-reporter=spec transport/*.test.mjs

test-multinode: dev-deps
	go build -o bin/streamkit-server-cluster ./server/cmd/streamkit-server
	STREAMKIT_TEST_DATABASE_URL='$(TEST_DB)' STREAMKIT_TEST_REDIS_URL='$(TEST_REDIS)' go test -count=1 -p 1 ./tests/cluster/...
	cd tests/interoperability && npm ci && npm run bundle && node --test --test-reporter=spec multinode.test.mjs

test-media: dev-deps
	cd tests/media && npm ci && npm test

# Real calls on loopback. They need pjsua, built as docs/deployment/sip.md says.
test-sip: dev-deps
	cd tests/sip && npm ci && npm test

# The server SDKs and the CLI against real servers: this one, and the reference server if its binary is there. They
# start what they need. STREAMKIT_TEST_WORKERS=1 adds recording and ingest, which need Chrome and ffmpeg. PYTHON is an
# interpreter with the Python SDK's test dependencies: pip install -e 'sdks/server-python[test]'.
test-sdks: dev-deps
	STREAMKIT_TEST_DATABASE_URL='$(TEST_DB)' go test -count=1 -p 1 ./sdks/... ./cli/...
	cd sdks/server-node && npm ci && STREAMKIT_TEST_DATABASE_URL='$(TEST_DB)' npm test
	cd sdks/server-python && STREAMKIT_TEST_DATABASE_URL='$(TEST_DB)' $(PYTHON) -m pytest

# Real packet impairment in containers, on this server and the reference. Emulated on one machine, not a real network.
test-network:
	CGO_ENABLED=0 go build -o bin/streamkit-server-static ./server/cmd/streamkit-server
	cd sdk-js && npm ci
	cd sdk-react && npm ci
	cd tests/interoperability && npm ci
	cd tests/network && npm test

lint:
	gofmt -l server api database tests/load tests/sdk egress ingress sip sdks cli
	go vet $(GO_PACKAGES)

load-samples:
	tests/load/make-samples.sh

clean:
	rm -rf bin

# Voice agents. They need the virtualenvs and engines described in streamkit-agents/local-engines/README.md.
test-agents:
	cd streamkit-agents/python && python -m pytest
	cd streamkit-agents/node && npm ci && node --test --test-force-exit --test-concurrency=1 tests/
	cd tests/agents && npm ci && npm test
