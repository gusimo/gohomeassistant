# GoHomeAssistant: write Home Assistant automations in Go.
#
# Build:  docker build -t gohomeassistant .
# Run:    ./start.sh   (or see README.md for docker run)

# 1. Web UI (React + Monaco), embedded into the Go binary.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web ./
RUN npm run build

# 2. GoHomeAssistant itself, plus gopls (code completion) and Delve (debugger).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/server/dist/app ./internal/server/dist/app
RUN CGO_ENABLED=0 go build -o /gohomeassistant ./cmd/gohomeassistant
# gopls v0.20 is the newest release that supports Go 1.24.
RUN CGO_ENABLED=0 GOBIN=/out go install golang.org/x/tools/gopls@v0.20.0
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/go-delve/delve/cmd/dlv@v1.25.2

# 3. Runtime. It keeps the Go toolchain: your projects are compiled in place.
FROM golang:1.24-alpine
# git: version control of projects. tini: reaps orphaned processes (PID 1).
RUN apk add --no-cache git tini
COPY --from=build /gohomeassistant /usr/local/bin/gohomeassistant
COPY --from=build /out/gopls /out/dlv /usr/local/bin/
COPY sdk /opt/gohomeassistant/sdk
# Build and module caches live in the data volume, so restarts are fast.
ENV GOCACHE=/data/gohomeassistant/cache \
    GOMODCACHE=/data/gohomeassistant/mod \
    CGO_ENABLED=0
# /data/projects: your projects. /data/gohomeassistant: builds, generated entities.
VOLUME /data
EXPOSE 8099
ENTRYPOINT ["/sbin/tini", "--", "gohomeassistant"]
