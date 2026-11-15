.PHONY: fmt test build run

fmt:
	gofmt -s -w internal/ cmd/

test:
	go test -v ./...

build:
	go build -o dance-scheduler ./cmd/scheduler

run:
	go run ./cmd/scheduler
