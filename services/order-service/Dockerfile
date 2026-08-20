# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#
#     docker build -f services/order-service/Dockerfile .
#
# Uniform across all thirteen images. Ten of them genuinely need it — a shared
# library under libs/ or contracts/ that a build context cannot reach above
# itself to find — and the remaining three follow suit so there is one rule
# rather than a per-image exception somebody has to remember.

# ---- build ------------------------------------------------------------------
# Must be >= the `go` directive in go.mod. It was 1.23 while the modules
# declared 1.25.0, so every Go image failed at `go mod download` — invisible
# locally, where the installed toolchain is newer than either.
FROM golang:1.25-alpine AS build

WORKDIR /src

# The shared model-check module is referenced by a `replace` directive, so it
# has to be in the context before `go mod download` runs — and at exactly
# ../../libs/go-modelcheck relative to WORKDIR, which /src makes /libs.
COPY libs/go-modelcheck /libs/go-modelcheck

# Dependencies next, so a code-only change reuses the module cache layer.
COPY services/order-service/go.mod services/order-service/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY services/order-service/ .

# CGO off gives a static binary that runs on a distroless base.
# -trimpath keeps build-host paths out of panics.
# The version is stamped in rather than read at runtime so a running pod can
# always be traced back to a commit.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/order-service ./cmd/server

# ---- test (built in CI, skipped for a plain image build) --------------------
FROM build AS test
RUN CGO_ENABLED=0 go test ./... && go vet ./...

# ---- runtime ----------------------------------------------------------------
# Distroless: no shell, no package manager, no busybox. If someone gets RCE in
# this container there is nothing in it to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/order-service /order-service
COPY --from=build /src/migrations /migrations

# nonroot (uid 65532) is baked into the base image, but state it explicitly so
# the Pod security context and the image cannot silently disagree.
USER 65532:65532

EXPOSE 8084

# No HEALTHCHECK: Kubernetes owns liveness and readiness via the probes in
# k8s/deployment.yaml, and a second, differently-configured health mechanism
# inside the image is a source of confusing double failures.

ENTRYPOINT ["/order-service"]
