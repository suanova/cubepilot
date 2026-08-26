# CubePilot API image (Portal REST/SSE + metadata store).
# Self-contained multi-stage build: the Go binary compiles inside Docker, so no
# host Go toolchain or host-built bin/ artifact is required (CI-friendly).
FROM harbor.isuanova.com/library/golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cubepilot-api ./cmd/cubepilot-api

FROM harbor.isuanova.com/library/debian:bookworm-slim
COPY --from=build /out/cubepilot-api /usr/local/bin/cubepilot-api
USER 1000:1000
ENTRYPOINT ["cubepilot-api"]
CMD []
