# CubePilot operator image (platform controllers).
#
# The Go binary is built on the HOST (scripts/setup.sh runs
# `CGO_ENABLED=0 go build -o bin/cubepilot-operator ./cmd/cubepilot-operator`) and copied in, reusing the
# already-cached openclaw:local base (Debian bookworm-slim) so we never pull a
# full Go toolchain image on slow/offline networks. Swap back to a golang
# multi-stage build if you need a self-contained CI build.
FROM openclaw:local

COPY bin/cubepilot-operator /usr/local/bin/cubepilot-operator
USER node
ENTRYPOINT ["cubepilot-operator"]
CMD []
