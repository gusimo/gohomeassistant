FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web ./
RUN npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/server/dist/app ./internal/server/dist/app
RUN CGO_ENABLED=0 go build -o /gohomeassistant ./cmd/gohomeassistant

# The runtime image keeps the Go toolchain: projects are compiled in place.
FROM golang:1.24-alpine
RUN apk add --no-cache git
COPY --from=build /gohomeassistant /usr/local/bin/gohomeassistant
COPY sdk /opt/gohomeassistant/sdk
# Persist build and module caches across container restarts.
ENV GOCACHE=/data/gohomeassistant/cache \
    GOMODCACHE=/data/gohomeassistant/mod \
    CGO_ENABLED=0
VOLUME /data
EXPOSE 8099
ENTRYPOINT ["gohomeassistant"]
