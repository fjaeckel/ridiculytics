VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-unit test-integration test-js test-cardinality lint cover run docker js clean

all: test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ridiculytics ./cmd/ridiculytics

test: test-unit test-integration test-js

test-unit:
	go vet ./...
	go test -race $(shell go list ./... | grep -v '/test$$')

# Wires the real components together over real listeners, and asserts on the
# deployment files.
test-integration:
	go test -race ./test/

# counter.js runs in a vm sandbox with a hand-rolled DOM: no test dependencies.
test-js:
	node --test web/counter.test.js

lint:
	golangci-lint run --timeout=5m

cover:
	go test -cover ./...

# Cardinality behaviour is the thing most likely to regress silently: if the
# marginals stop being exact, every dashboard quietly starts lying.
test-cardinality:
	go test -race -v -run 'Marginal|Cardinality|PathAdmission|ASNFolds|Squatting|Flood|Bounded' \
		./internal/aggregate/

run: build
	./bin/ridiculytics -config sites.yaml

js:
	npm install && npm run build
	@printf 'counter.min.js gzipped: ' && gzip -c web/counter.min.js | wc -c

docker:
	docker build --build-arg VERSION=$(VERSION) -t ridiculytics:$(VERSION) .

clean:
	rm -rf bin web/counter.min.js
