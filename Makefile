.PHONY: build vet test race run clean

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

run:
	go run ./cmd/aiharn

clean:
	go clean
