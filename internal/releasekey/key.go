// Package releasekey holds the Ed25519 public key that release artifacts are
// verified against. It lives in its own package so that BOTH the self-updater
// (cmd) and the release tooling (tools/verify, run in CI) use the exact same
// key — a release can therefore never ship a signature the shipped binaries
// would reject.
//
// Public is empty for a fork that hasn't set up signing; when it is non-empty,
// `calvoproxy update` REQUIRES a valid signature (fail-closed).
//
// It is a var (not a const) so a fork can stamp its own key at build time:
//
//	go build -ldflags "-X github.com/cervantesh/calvoproxy/internal/releasekey.Public=<base64>"
package releasekey

var Public = "GZXKveslK0EYlHea9EUpcdnWo6QIXmwE9ksfhq2gups="
