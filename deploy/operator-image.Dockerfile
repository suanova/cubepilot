# CubePilot operator image (platform controllers).
# Self-contained multi-stage build: the Go binary compiles inside Docker, so no
# host Go toolchain or host-built bin/ artifact is required (CI-friendly).
FROM harbor.isuanova.com/library/golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cubepilot-operator ./cmd/cubepilot-operator

FROM harbor.isuanova.com/library/node:24-bookworm-slim
COPY --from=build /out/cubepilot-operator /usr/local/bin/cubepilot-operator
USER node
ENTRYPOINT ["cubepilot-operator"]
CMD []
