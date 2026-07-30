FROM golang:1.25-alpine AS builder
WORKDIR /app
# Dependencies are vendored (vendor/), so the build is fully offline —
# no `go mod download` and no network access required.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -o calvoproxy ./cmd

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/calvoproxy .
# Editable model chains — override by mounting your own over /app/model-policy.json.
COPY --from=builder /app/model-policy.json .
EXPOSE 8080
ENV PORT=8080 GRPC_PORT=9090
# OPENROUTER_API_KEY must be supplied at runtime:
#   docker run -e OPENROUTER_API_KEY=sk-or-v1-... -p 8080:8080 calvoproxy
CMD ["./calvoproxy"]
