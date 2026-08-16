# CubePilot assistant service + Instance Manager image.
#
# The Go binary is built on the HOST (scripts/setup.sh runs
# `CGO_ENABLED=0 go build -o bin/cubepilot`) and copied in, reusing the
# already-cached openclaw:local base (Debian bookworm-slim) so we never pull a
# full Go toolchain image on slow/offline networks. Swap back to a golang
# multi-stage build if you need a self-contained CI build.
FROM openclaw:local

COPY bin/cubepilot /usr/local/bin/cubepilot
USER node
ENTRYPOINT ["cubepilot"]
CMD []
