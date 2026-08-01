FROM golang:1.26-alpine AS builder
WORKDIR /app
# VERSION is stamped into the binary so it can report itself and detect updates.
ARG VERSION=dev
# Dependencies are vendored (vendor/), so the build is fully offline —
# no `go mod download` and no network access required.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -ldflags "-X main.version=${VERSION}" -o calvoproxy ./cmd

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/calvoproxy .
# Editable model chains — override by mounting your own over /app/model-policy.json.
COPY --from=builder /app/model-policy.json .
EXPOSE 8080
ENV PORT=8080
# The binary now defaults HOST to 127.0.0.1 (loopback) for host installs; a
# container must bind all interfaces to be reachable via -p, so set it here.
ENV HOST=0.0.0.0
# NOTE: the image deliberately does NOT set PROXY_ALLOW_ENV_KEY_PUBLIC. Because
# it binds 0.0.0.0, allowing keyless clients to spend the container's
# OPENROUTER_API_KEY would make any reachable instance an open relay on the
# operator's bill. Clients should send their own key; if you really want the
# container to spend its ambient key for keyless clients (e.g. a private LAN),
# opt in explicitly with -e PROXY_ALLOW_ENV_KEY_PUBLIC=true, and set
# PROXY_ADMIN_TOKEN so /health and /metrics aren't world-readable.
# Marks the runtime as containerized so the update notice recommends pulling a
# new image (rather than `calvoproxy update`, which can't swap a container's fs).
ENV CALVOPROXY_CONTAINER=1
# OPENROUTER_API_KEY must be supplied at runtime:
#   docker run -e OPENROUTER_API_KEY=sk-or-v1-... -p 8080:8080 calvoproxy
CMD ["./calvoproxy"]
