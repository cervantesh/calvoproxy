<!-- Target this PR at `dev`, not `main`.

     This repo publishes fast, on purpose. This checklist is not a brake on
     that — it is the small set of things that, when skipped, produced the
     same-day regressions that cost more time than they saved. -->

## What & why

<!-- The failure this fixes, or the capability this adds. If it came from a
     real incident, say what the user actually saw. Link issues: Fixes #123 -->

## How it was verified

<!-- Name the check. "Tests pass" is not a verification. "The new test fails
     when I revert the fix" is. -->

## Checklist

- [ ] PR targets `dev`
- [ ] `go build -mod=vendor ./...` succeeds
- [ ] `go test ./... -race` passes
- [ ] `./scripts/coverage-gate.sh` passes — or the floors change is in this diff and explained above
- [ ] `./scripts/vendor-manifest.sh` passes — or the vendored change is explained above
- [ ] The new test **fails without the fix**, mutation-checked rather than assumed
- [ ] This change adds no new assumption about what OpenRouter does — or that assumption is pinned in `test/contract/`
- [ ] `CHANGELOG.md` updated under `## [Unreleased]`
- [ ] Docs/README (`README.md` **and** `README.es.md`) updated if behavior changed
- [ ] No secrets, private hostnames, or personal paths added
- [ ] I did **not** run `go mod tidy` / `go get` (deps are vendored; see [THIRD_PARTY.md](../THIRD_PARTY.md))
- [ ] **All** CI checks are green — every one, not the first that reported
- [ ] Reviewed by someone other than the author, **or** the self-merge is justified below

<!-- Why these specific boxes:

     - "fails without the fix": a test that passes either way certifies nothing
       and still looks like coverage. Verified the hard way here — an ordering
       assertion using httptest.ResponseRecorder passed even with the ordering
       inverted, because the recorder never snapshots headers.

     - "no new upstream assumption": mocks encode assumptions. The 64-tool 400
       shipped because no mock modelled it, the unit suite stayed green, and
       every agent turn reaching the capped provider died in 0.8s.

     - "all CI checks green": v0.5.1 was merged with a red check because one
       green check was taken as the answer.

     - "reviewed by someone other than the author": every release to date was
       authored and merged by one actor. Self-merging is allowed; self-merging
       silently is what this box is meant to stop. -->

Self-merge justification (if applicable):

## Blast radius

- [ ] If this is wrong in production the failure is **loud** — an error, a metric, a log line — not a silent degradation

<!-- Silent degradation is this proxy's characteristic failure mode: a chain
     that quietly answers from a weaker model. That is why the served-model
     headers exist. Ask whether your change can degrade quietly, and make it
     announce itself if it can. -->
