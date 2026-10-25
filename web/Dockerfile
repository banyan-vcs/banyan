# Build stage: compile the TypeScript web service.
# npm registry can be pointed at a mirror for constrained networks:
#   docker build --build-arg NPM_REGISTRY=https://registry.npmmirror.com -t banyan-web web/
FROM node:22-alpine AS build
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /src
COPY package.json tsconfig.json ./
RUN npm ci --registry=$NPM_REGISTRY || npm install --registry=$NPM_REGISTRY
COPY src ./src
RUN npm run build

# Runtime stage: static output only — the devDependencies never ship.
FROM node:22-alpine
WORKDIR /app
COPY --from=build /src/dist ./dist
ENV PORT=3000 BANYAN_URL=http://banyan:8080
EXPOSE 3000
CMD ["node", "dist/server.js"]
