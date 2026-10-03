# syntax=docker/dockerfile:1
# One image carries every binary; each Compose service picks its command.
# The last stage (static, CGO off) is the default. The `tigerbeetle` target
# builds with the TigerBeetle engine (A§9.7), which needs cgo and glibc.

FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/ ./cmd/api ./cmd/worker ./cmd/seed ./cmd/checker

FROM golang:1.27 AS build-tigerbeetle
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -tags tigerbeetle -o /out/ ./cmd/api ./cmd/worker ./cmd/seed ./cmd/checker

# glibc must be at least the build image's (golang:1.27 is Debian 13, glibc 2.41).
FROM gcr.io/distroless/base-debian13 AS tigerbeetle
COPY --from=build-tigerbeetle /out/ /usr/local/bin/
USER nonroot

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
USER nonroot
