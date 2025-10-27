.PHONY: build run test tidy vet

build:
	GO111MODULE=on go build ./...

run:
	GO111MODULE=on go run ./cmd/s3bee

test:
	GO111MODULE=on go test ./...

vet:
	GO111MODULE=on go vet ./...

tidy:
	GO111MODULE=on go mod tidy
