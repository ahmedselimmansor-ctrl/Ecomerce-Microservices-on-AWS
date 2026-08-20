# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#
#     docker build -f services/cart-service/Dockerfile .
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
COPY services/cart-service/package*.json ./services/cart-service/
WORKDIR /repo/services/cart-service
RUN npm install --omit=dev --no-audit --no-fund

FROM node:22-alpine AS build
WORKDIR /repo
COPY libs/ts-contracts ./libs/ts-contracts
COPY services/cart-service/package*.json ./services/cart-service/
WORKDIR /repo/services/cart-service
RUN npm install --no-audit --no-fund
COPY services/cart-service/ .
RUN npm run build

FROM node:22-alpine AS dev
WORKDIR /repo
COPY libs/ts-contracts ./libs/ts-contracts
COPY services/cart-service/package*.json ./services/cart-service/
WORKDIR /repo/services/cart-service
RUN npm install --no-audit --no-fund
COPY services/cart-service/ .
EXPOSE 8083
CMD ["npm","run","dev"]

FROM node:22-alpine AS runtime
# Alpine ships a `node` user at uid 1000. Using it rather than creating one
# keeps the uid stable across base image updates.
WORKDIR /repo/services/cart-service
# The library comes too. node_modules holds a symlink into ../../libs, so
# without the target every require of @souq/contracts fails at startup — and it
# fails on the first request, not at boot, because the import is lazy.
COPY --from=deps  --chown=node:node /repo/libs/ts-contracts /repo/libs/ts-contracts
COPY --from=deps  --chown=node:node /repo/services/cart-service/node_modules ./node_modules
COPY --from=build --chown=node:node /repo/services/cart-service/dist ./dist
COPY --chown=node:node services/cart-service/package.json ./
USER node
EXPOSE 8083
# --enable-source-maps so a production stack trace points at TypeScript.
ENV NODE_OPTIONS="--enable-source-maps"
CMD ["node","dist/index.js"]
