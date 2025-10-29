.PHONY: build run run-local run-sigv4 test test-verbose tidy vet

build:
	GO111MODULE=on go build ./...

run:
	GO111MODULE=on go run ./cmd/s3free

run-local:
	GO111MODULE=on S3FREE_CONFIG=configs/local.yaml go run ./cmd/s3free

run-sigv4:
	GO111MODULE=on S3FREE_CONFIG=configs/local.yaml S3FREE_AUTH_MODE=sigv4 S3FREE_ACCESS_KEYS="AKIAEXAMPLE:secret:local" go run ./cmd/s3free

test:
	GO111MODULE=on go test ./...

test-verbose:
	GO111MODULE=on go test -v ./...

vet:
	GO111MODULE=on go vet ./...

tidy:
	GO111MODULE=on go mod tidy
