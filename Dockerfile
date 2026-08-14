# golang:1.26-alpine3.22 (pinned by manifest digest, not mutable tag).
FROM golang:1.26-alpine3.22@sha256:727cfc3c40be55cd1bc9a4a059406b28a059857e3be752aa9d09531e12c20c56 AS builder
WORKDIR /app
# VERSION is stamped into the binary so it can report itself and detect updates.
ARG VERSION=dev
# Dependencies are vendored (vendor/), so the build is fully offline —
# no `go mod download` and no network access required.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -ldflags "-X main.version=${VERSION}" -o calvoproxy ./cmd

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 calvoproxy \
    && adduser -S -D -H -u 65532 -G calvoproxy calvoproxy
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
# Learned per-model reliability scores live here, and the path is pinned rather
# than left to the default.
#
# The default is <user-config-dir>/calvoproxy/scores.json, which on Linux
# resolves through $XDG_CONFIG_HOME or $HOME. Docker always defines HOME (/root,
# or / for a --user UID absent from /etc/passwd), so the store does not go
# pathless here — but under `--user` that default lands on /.config, which the
# process cannot create: measured, it fails every flush with
# "mkdir /.config: permission denied". Pinning a path we control removes the
# question entirely.
#
# 1777 (sticky, like /tmp) rather than 755: a volume mounted here inherits these
# bits, and with `--user 1234` a root-owned 755 directory is not writable — also
# measured, "open /data/.scores-*.tmp: permission denied". Sticky keeps one UID
# from deleting another's files, and the store itself writes the score file 0600.
#
# Mount a volume at /data to make scores outlive the container:
#   docker run -v calvoproxy-scores:/data ...
# Without one they still work for the container's lifetime, but are lost on
# recreation. No VOLUME directive here on purpose — it would litter an anonymous
# volume per `docker run`; docker-compose.yml declares a named one instead.
ENV PROXY_SCORE_FILE=/data/scores.json
ENV HOME=/data
RUN mkdir -p /data && chown -R 65532:65532 /app /data && chmod 0700 /data
# OPENROUTER_API_KEY must be supplied at runtime:
#   docker run -e OPENROUTER_API_KEY=sk-or-v1-... -p 8080:8080 calvoproxy
USER 65532:65532
CMD ["./calvoproxy"]
