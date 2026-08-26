# CubePilot Portal image — multi-stage: build the Vue SPA on node, serve it
# with nginx (which also reverse-proxies /api to the cubepilot-api service).
#
# Build:  docker build -t harbor.isuanova.com/cubestack/cubepilot-web:<tag> -f web/Dockerfile web
FROM harbor.isuanova.com/library/node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM harbor.isuanova.com/library/nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
