.PHONY: check format test

format:
	gofmt -w ./cmd ./internal

test:
	go test -race ./...

check:
	test -z "$$(gofmt -l ./cmd ./internal)"
	go vet ./...
	go test -race ./...
	./tests/static.sh
