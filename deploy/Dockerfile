# RouteGate gateway image.
# The binary is cross-compiled on the host (CGO off, static) and copied into a
# scratch image — no base image pull needed. See scripts/build-demo.sh.
#
# QUICKSTART/DEMO ONLY: this Dockerfile builds the demo image (static binary
# into FROM scratch — no shell, no curl). Production images are built from the
# repository-root Dockerfile by the Release Console (alpine runtime with curl
# for the healthcheck).
FROM scratch

ARG TARGETARCH=arm64
COPY deploy/bin/routegate-linux-${TARGETARCH} /routegate

# Data plane (mTLS) / admin (M3) / health (plaintext probes).
EXPOSE 8080 8081 8082

ENTRYPOINT ["/routegate"]
CMD ["serve"]
