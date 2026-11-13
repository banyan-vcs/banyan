# The banyan web tier image: the SPA (vite) + the banyand binary, served
# by `banyand web` (internal/webapp). Three stages — node builds the SPA,
# golang builds the daemon (GOPROXY is an arg so constrained networks can
# point it at a mirror), the runtime is alpine with the binary and dist/
# only. BASE_PATH bakes the deployment prefix into the SPA (GitLab's
# /-/banyan subpage); the web tier strips it at request time.
#
#   docker build -f web/Dockerfile --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/ -t banyan-web .
FROM node:22-alpine AS spa
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG BASE_PATH=
WORKDIR /src
COPY web/package.json web/package-lock.json web/tsconfig.json web/vite.config.ts web/index.html ./
RUN npm ci --registry=$NPM_REGISTRY || npm install --registry=$NPM_REGISTRY
COPY web/public ./public
COPY web/src ./src
RUN BASE_PATH=$BASE_PATH npm run build

FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/banyand ./cmd/banyand

FROM alpine:3.21
WORKDIR /app
ENV PORT=3000 BANYAN_URL=http://banyan:8080 DIST_DIR=/app/dist NODE_ENV=production
COPY --from=build /out/banyand /usr/local/bin/banyand
COPY --from=spa /src/dist ./dist
EXPOSE 3000
ENTRYPOINT ["banyand", "web"]
CMD ["-listen", "0.0.0.0:3000"]
