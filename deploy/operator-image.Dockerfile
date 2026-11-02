# CubePilot operator image (platform controllers).
# Self-contained multi-stage build: the Go binary compiles inside Docker, so no
# host Go toolchain or host-built bin/ artifact is required (CI-friendly).
FROM harbor.isuanova.com/library/golang:1.26-bookworm AS build
# Pin a patched Go patch release without re-mirroring the toolchain image.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cubepilot-operator ./cmd/cubepilot-operator

FROM scratch
# The binary is static, so there is no OS userland to patch; keep the CA bundle
# for outbound TLS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/cubepilot-operator /usr/local/bin/cubepilot-operator
USER 1000:1000
ENTRYPOINT ["cubepilot-operator"]
CMD []
