.PHONY: check format test static

format:
	gofmt -w ./cmd ./internal

test:
	go test -race ./...

static:
	./tests/static.sh

check:
	test -z "$$(gofmt -l ./cmd ./internal)"
	go vet ./...
	go test -race ./...
	$(MAKE) static
