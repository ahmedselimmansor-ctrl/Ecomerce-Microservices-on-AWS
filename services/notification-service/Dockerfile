# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#
#     docker build -f services/notification-service/Dockerfile .
#
# And the repo layout is REPLICATED inside the image, under /repo. That is not
# tidiness — package-lock.json records the shared library as
# `file:../../libs/ts-contracts`, a path relative to the package. Flatten the
# layout and npm resolves it to somewhere that does not exist, then fails with
# `Cannot read properties of undefined (reading 'extraneous')`, which names
# neither the package nor the path.
#
# So: same relative positions as on a developer's machine, and the lockfile
# resolves identically in both.

FROM node:22-alpine AS deps
WORKDIR /repo
COPY libs/ts-contracts ./libs/ts-contracts
# BUILD it. `main` points at dist/, which is gitignored — so a clean checkout
# has no dist/ and every consumer fails with "Cannot find module
# '@souq/contracts'". A local build can pass on a stale dist/ left over from
# earlier work, which is exactly how this reached CI.
RUN cd libs/ts-contracts && npm install --no-audit --no-fund && npm run build
COPY services/notification-service/package*.json ./services/notification-service/
WORKDIR /repo/services/notification-service
RUN npm install --omit=dev --no-audit --no-fund

FROM node:22-alpine AS build
WORKDIR /repo
COPY --from=deps /repo/libs/ts-contracts ./libs/ts-contracts
COPY services/notification-service/package*.json ./services/notification-service/
WORKDIR /repo/services/notification-service
RUN npm install --no-audit --no-fund
COPY services/notification-service/ .
RUN npm run build

FROM node:22-alpine AS dev
WORKDIR /repo
COPY libs/ts-contracts ./libs/ts-contracts
RUN cd libs/ts-contracts && npm install --no-audit --no-fund && npm run build
COPY services/notification-service/package*.json ./services/notification-service/
WORKDIR /repo/services/notification-service
RUN npm install --no-audit --no-fund
COPY services/notification-service/ .
EXPOSE 8091
CMD ["npm","run","dev"]

FROM node:22-alpine AS runtime
# Alpine ships a `node` user at uid 1000. Using it rather than creating one
# keeps the uid stable across base image updates.
WORKDIR /repo/services/notification-service
# The library comes too. node_modules holds a symlink into ../../libs, so
# without the target every require of @souq/contracts fails at startup — and it
# fails on the first request, not at boot, because the import is lazy.
COPY --from=deps  --chown=node:node /repo/libs/ts-contracts /repo/libs/ts-contracts
COPY --from=deps  --chown=node:node /repo/services/notification-service/node_modules ./node_modules
COPY --from=build --chown=node:node /repo/services/notification-service/dist ./dist
COPY --chown=node:node services/notification-service/package.json ./
USER node
EXPOSE 8091
# --enable-source-maps so a production stack trace points at TypeScript.
ENV NODE_OPTIONS="--enable-source-maps"
CMD ["node","dist/index.js"]
