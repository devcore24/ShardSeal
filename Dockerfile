# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.22-alpine AS build
RUN apk add --no-cache ca-certificates tzdata upx
WORKDIR /src

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build static binary
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /out/shardseal ./cmd/shardseal \
 && upx -q /out/shardseal || true

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -h /home/app app
USER app
WORKDIR /home/app

# Binary
COPY --from=build /out/shardseal /usr/local/bin/shardseal

# Data and config mount points
RUN mkdir -p /home/app/data /home/app/config
VOLUME ["/home/app/data", "/home/app/config"]

# Ports:
# - 8080: S3 data plane (address in config.yaml or SHARDSEAL_ADDR)
# - 9090: optional Admin API when configured (AdminAddress)
EXPOSE 8080 9090

# Defaults (can be overridden by env)
ENV SHARDSEAL_ADDR=":8080"

# To use a config file, mount it and set:
#  -v $(pwd)/configs/local.yaml:/home/app/config/config.yaml
#  -e SHARDSEAL_CONFIG=/home/app/config/config.yaml
ENTRYPOINT ["/usr/local/bin/shardseal"]