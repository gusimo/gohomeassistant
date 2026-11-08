FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
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
ENTRYPOINT ["gohomeassistant"]
