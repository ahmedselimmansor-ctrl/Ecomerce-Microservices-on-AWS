# syntax=docker/dockerfile:1.7

# ---- build ------------------------------------------------------------------
# Must be >= the `go` directive in go.mod. It was 1.23 while the modules
# declared 1.25.0, so every Go image failed at `go mod download` — invisible
# locally, where the installed toolchain is newer than either.
FROM golang:1.25-alpine AS build
WORKDIR /src

# The shared model-check module is referenced by a `replace` directive, so it
# has to be in the context before `go mod download` runs.
COPY libs/go-modelcheck /libs/go-modelcheck
COPY services/inventory-service/go.mod services/inventory-service/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY services/inventory-service/ .

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/inventory-service ./cmd/server

# ---- test (built in CI, skipped for a plain image build) --------------------
FROM build AS test
# -race needs cgo, so it runs in CI on the host rather than here.
RUN CGO_ENABLED=0 go test ./... && go vet ./...

# ---- runtime ----------------------------------------------------------------
# Distroless: no shell, no package manager. If someone gets RCE in this
# container there is nothing in it to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/inventory-service /inventory-service
COPY --from=build /src/migrations /migrations

USER 65532:65532
EXPOSE 8085

# No HEALTHCHECK: Kubernetes owns liveness and readiness, and a second,
# differently-configured mechanism inside the image causes confusing double
# failures.
ENTRYPOINT ["/inventory-service"]
