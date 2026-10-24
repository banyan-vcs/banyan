# Build stage: compile the daemon and the thin client as static binaries.
# GOPROXY is a build arg so constrained networks can point it at a mirror:
#   docker build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/ -t banyan .
FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/banyand ./cmd/banyand \
 && CGO_ENABLED=0 go build -trimpath -o /out/banyan ./cmd/banyan

# Runtime stage: one image carries banyand (server) and banyan (thin CLI).
# State lives under /var/lib/banyan — SQLite metadata plus pack shards —
# and survives container re-creation via a mounted volume.
FROM alpine:3.21
COPY --from=build /out/banyand /usr/local/bin/banyand
COPY --from=build /out/banyan /usr/local/bin/banyan
VOLUME /var/lib/banyan
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/ >/dev/null || exit 1
ENTRYPOINT ["banyand"]
# Extra args append to these, e.g.:
#   docker run banyan -init-repo demo
CMD ["-listen", "0.0.0.0:8080", "-root", "/var/lib/banyan", "-meta", "sqlite"]
