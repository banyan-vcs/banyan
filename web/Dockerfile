# Build stage: the React SPA (vite) + the session/proxy BFF (tsc, commonjs).
# npm registry can be pointed at a mirror for constrained networks:
#   docker build --build-arg NPM_REGISTRY=https://registry.npmmirror.com -t banyan-web web/
# BASE_PATH bakes the deployment prefix into the SPA (e.g. /-/banyan for the
# GitLab subpage deployment).
FROM node:22-alpine AS build
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG BASE_PATH=
WORKDIR /src
COPY package.json package-lock.json tsconfig.json tsconfig.server.json vite.config.ts index.html ./
RUN npm ci --registry=$NPM_REGISTRY || npm install --registry=$NPM_REGISTRY
COPY server ./server
COPY public ./public
COPY src ./src
RUN BASE_PATH=$BASE_PATH npm run build

# Runtime stage: dist/ (SPA) + server-dist/ (BFF) only — the devDependencies
# never ship; the BFF itself has zero runtime dependencies.
FROM node:22-alpine
WORKDIR /app
ENV PORT=3000 BANYAN_URL=http://banyan:8080 NODE_ENV=production
COPY --from=build /src/dist ./dist
COPY --from=build /src/server-dist ./server-dist
EXPOSE 3000
CMD ["node", "server-dist/server.js"]
