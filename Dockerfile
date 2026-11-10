# syntax=docker/dockerfile:1

# Cross-compile on the build platform; no QEMU emulation needed for the Go build.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/echo-server ./cmd/echo-server

# Distroless: CA certs, tzdata and a nonroot user, nothing else.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /out/echo-server /echo-server

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=2s --retries=3 \
  CMD ["/echo-server", "healthcheck"]

ENTRYPOINT ["/echo-server"]
