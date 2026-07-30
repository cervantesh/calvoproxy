# Contributing to CalvoProxy

Thanks for your interest in improving CalvoProxy! This guide covers how to build,
test, and submit changes.

## Ground rules

- Be respectful — see the [Code of Conduct](CODE_OF_CONDUCT.md).
- Open an issue before large changes so we can align on direction.
- Keep pull requests focused; one logical change per PR.

## Branching model

- `main` — protected, always releasable. No direct pushes.
- `dev` — integration branch. **Target your PRs at `dev`.**
- Feature branches — `feat/<short-name>`, `fix/<short-name>`, etc., off `dev`.

Releases are cut from `main` after changes graduate from `dev`.

## Build & test

Dependencies are **vendored** (`vendor/`), so everything builds offline:

```bash
go build -mod=vendor -o calvoproxy ./cmd     # build
go test ./...                                # test
GOPROXY=off go build ./cmd                   # prove it's fully offline
```

> Do **not** run `go mod tidy` / `go get`. Some dependencies come from a private
> registry that isn't publicly resolvable; the vendored copies are the source of
> truth. Dependency changes are made upstream and re-vendored.

Please run `gofmt` (or `go fmt ./...`) and ensure `go vet ./...` and `go test ./...`
pass before opening a PR.

## Changing the free-model chains

The per-profile model chains live in [`model-policy.json`](model-policy.json) and
are editable without recompiling — no code change needed. Only touch the embedded
default (`internal/router/config/model-policy.default.json`) when you intend to
change the built-in fallback baseline.

## Commit messages

Use clear, imperative subject lines (e.g. "Add idle self-shutdown"). Reference
issues where relevant (`Fixes #123`).

## Pull request checklist

- [ ] PR targets `dev`
- [ ] `go build -mod=vendor ./...` succeeds
- [ ] `go test ./...` passes
- [ ] Docs/README updated if behavior changed
- [ ] No secrets, private hostnames, or personal paths added

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
