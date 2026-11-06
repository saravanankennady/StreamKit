# Builds a Next.js app in this repository. From the repository root:
#   docker build -f infrastructure/docker/next-app.Dockerfile --build-arg APP=dashboard .
ARG APP=dashboard
FROM node:22-alpine AS build
ARG APP
WORKDIR /src
COPY . .
WORKDIR /src/${APP}
RUN npm ci && npm run build

FROM node:22-alpine
ARG APP
ENV NODE_ENV=production PORT=3000
WORKDIR /app
COPY --from=build /src/${APP}/.next/standalone ./
COPY --from=build /src/${APP}/.next/static ./${APP}/.next/static
COPY --from=build /src/${APP}/public ./${APP}/public
USER node
EXPOSE 3000
ENV APP=${APP}
CMD ["sh", "-c", "node ${APP}/server.js"]
