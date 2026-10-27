# CubePilot operator image (platform controllers).
# Self-contained multi-stage build: the Go binary compiles inside Docker, so no
# host Go toolchain or host-built bin/ artifact is required (CI-friendly).
FROM harbor.isuanova.com/library/golang:1.26-bookworm AS build
# Pin the patched toolchain from go.mod without re-mirroring this image.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cubepilot-operator ./cmd/cubepilot-operator

FROM harbor.isuanova.com/library/debian:bookworm-slim
# Apply bookworm security updates; the mirrored base image lags Debian point releases.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/cubepilot-operator /usr/local/bin/cubepilot-operator
USER 1000:1000
ENTRYPOINT ["cubepilot-operator"]
CMD []
