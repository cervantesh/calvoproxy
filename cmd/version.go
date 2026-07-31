package main

import "os"

// version is the running build's version. It is "dev" for local/unversioned
// builds and is stamped at release time via
//
//	go build -ldflags "-X main.version=vX.Y.Z"
//
// The self-update and update-check logic below only act when this is a real
// (non-"dev") version, so a developer build never nags or tries to replace
// itself.
var version = "dev"

// repoSlug is the GitHub owner/repo whose releases back the update check and
// self-update. Overridable at build time for forks:
//
//	go build -ldflags "-X main.repoSlug=you/yourfork"
var repoSlug = "cervantesh/calvoproxy"

// isDevBuild reports whether this is an unversioned local build, for which the
// update machinery stays silent.
func isDevBuild() bool { return version == "" || version == "dev" }

// inContainer reports whether we are running inside a container image. The
// Docker image sets CALVOPROXY_CONTAINER=1; we also fall back to the classic
// /.dockerenv probe. It changes only the *advice* the update notice prints
// (pull a new image vs. run `calvoproxy update`).
func inContainer() bool {
	if os.Getenv("CALVOPROXY_CONTAINER") != "" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}
