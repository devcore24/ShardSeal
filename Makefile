.PHONY: build run run-local run-sigv4 test test-verbose tidy vet

build:
	GO111MODULE=on go build ./...

run:
	GO111MODULE=on go run ./cmd/shardseal

run-local:
	GO111MODULE=on SHARDSEAL_CONFIG=configs/local.yaml go run ./cmd/shardseal

run-sigv4:
	GO111MODULE=on SHARDSEAL_CONFIG=configs/local.yaml SHARDSEAL_AUTH_MODE=sigv4 SHARDSEAL_ACCESS_KEYS="AKIAEXAMPLE:secret:local" go run ./cmd/shardseal

test:
	GO111MODULE=on go test ./...

test-verbose:
	GO111MODULE=on go test -v ./...

vet:
	GO111MODULE=on go vet ./...

tidy:
	GO111MODULE=on go mod tidy
