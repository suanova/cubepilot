# CubePilot API image (Portal + REST/SSE).
#
# The Go binary is built on the HOST (scripts/setup.sh runs
# `CGO_ENABLED=0 go build -o bin/cubepilot-api ./cmd/cubepilot-api`) and copied in, reusing the
# already-cached openclaw:local base (Debian bookworm-slim) so we never pull a
# full Go toolchain image on slow/offline networks. Swap back to a golang
# multi-stage build if you need a self-contained CI build.
FROM openclaw:local

COPY bin/cubepilot-api /usr/local/bin/cubepilot-api
USER node
ENTRYPOINT ["cubepilot-api"]
CMD []
