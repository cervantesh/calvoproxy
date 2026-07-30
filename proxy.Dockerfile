FROM golang:1.25-alpine AS builder
WORKDIR /app
# Dependencies are vendored (vendor/), so the build is fully offline —
# no `go mod download` and no network access required.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -o cervoproxy ./cmd

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/cervoproxy .
EXPOSE 8080
CMD ["./cervoproxy"]
