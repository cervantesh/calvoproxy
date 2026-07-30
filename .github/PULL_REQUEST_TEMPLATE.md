<!-- Target this PR at `dev`, not `main`. -->

## What & why

<!-- Describe the change and the motivation. Link issues: Fixes #123 -->

## Checklist

- [ ] PR targets `dev`
- [ ] `go build -mod=vendor ./...` succeeds
- [ ] `go test ./...` passes
- [ ] Docs/README updated if behavior changed
- [ ] No secrets, private hostnames, or personal paths added
- [ ] I did **not** run `go mod tidy` / `go get` (deps are vendored)
