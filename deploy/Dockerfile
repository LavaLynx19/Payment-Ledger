# syntax=docker/dockerfile:1
# One image carries every binary; each Compose service picks its command.
FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/ ./cmd/api ./cmd/worker ./cmd/seed ./cmd/checker

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
USER nonroot
