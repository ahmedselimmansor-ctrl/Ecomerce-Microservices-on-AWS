# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#
#     docker build -f apps/admin/Dockerfile .
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
# The contracts package publishes from dist/, so it has to be built before any
# consumer resolves it — `main` points there and npm does not run the build.
RUN cd libs/ts-contracts && npm install --no-audit --no-fund && npm run build
COPY apps/admin/package*.json ./apps/admin/
WORKDIR /repo/apps/admin
RUN npm install --no-audit --no-fund

FROM node:22-alpine AS build
WORKDIR /repo
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /repo/libs/ts-contracts ./libs/ts-contracts
COPY --from=deps /repo/apps/admin/node_modules ./apps/admin/node_modules
COPY apps/admin/ ./apps/admin/
WORKDIR /repo/apps/admin
# Standalone output: Next traces the real import graph and emits a server
# bundle with only the modules it needs — roughly a tenth of the image size of
# copying node_modules.
RUN npm run build

FROM node:22-alpine AS dev
WORKDIR /repo
COPY libs/ts-contracts ./libs/ts-contracts
RUN cd libs/ts-contracts && npm install --no-audit --no-fund && npm run build
COPY apps/admin/package*.json ./apps/admin/
WORKDIR /repo/apps/admin
RUN npm install --no-audit --no-fund
COPY apps/admin/ .
EXPOSE 3001
CMD ["npm","run","dev"]

FROM node:22-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3001
# standalone/ is flat, not nested under apps/admin/. Next nests it only when it
# traces a workspace root above the app — a root package.json or lockfile — and
# there is neither here. Verified by listing the directory rather than assumed:
# guessing wrong produces "not found" at COPY time with no hint which layout
# was actually emitted.
COPY --from=build --chown=node:node /repo/apps/admin/.next/standalone/ ./
COPY --from=build --chown=node:node /repo/apps/admin/.next/static ./.next/static
COPY --from=build --chown=node:node /repo/apps/admin/public ./public
# The ISR cache directory. The pod mounts an emptyDir here because the root
# filesystem is read-only; without the directory existing Next throws EROFS on
# the first revalidation and swallows it, so the symptom is "revalidation
# silently never happens" rather than an error.
RUN mkdir -p .next/cache && chown node:node .next/cache
USER node
EXPOSE 3001
CMD ["node","server.js"]
