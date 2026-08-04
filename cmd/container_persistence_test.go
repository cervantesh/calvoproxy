package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Container score persistence is held together by two files that must agree,
// and when they stop agreeing NOTHING fails: the proxy starts, serves, and
// quietly relearns its models from scratch on every recreation. That is the
// exact defect 0.9.2 fixed on the host and left standing in the container.
//
// So these tests assert the agreement itself. They are deliberately about
// content, not behaviour, because there is no behaviour to observe — the
// failure mode is silence.

// repoFile reads a file from the repo root (tests run in ./cmd).
func repoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// scoreFilePath in the router returns "" when it cannot determine a config dir,
// and on Linux that dir comes from $XDG_CONFIG_HOME or $HOME — neither of which
// a container is guaranteed to define. The image must therefore pin an absolute
// path rather than inherit the default.
func TestDockerfilePinsAnAbsoluteScorePath(t *testing.T) {
	dockerfile := repoFile(t, "Dockerfile")
	const want = "ENV PROXY_SCORE_FILE="
	idx := strings.Index(dockerfile, want)
	if idx < 0 {
		t.Fatal("the image must pin PROXY_SCORE_FILE; without it the store silently " +
			"disables itself whenever the container defines neither XDG_CONFIG_HOME nor HOME")
	}
	value := strings.TrimSpace(strings.SplitN(dockerfile[idx+len(want):], "\n", 2)[0])
	if !strings.HasPrefix(value, "/") {
		t.Fatalf("PROXY_SCORE_FILE must be an absolute path, got %q", value)
	}
	if strings.Contains(value, "$") {
		t.Fatalf("PROXY_SCORE_FILE must not depend on the environment, got %q", value)
	}
	// The directory has to exist and be writable, or the first flush fails.
	dir := pathDir(value)
	if !strings.Contains(dockerfile, "mkdir -p "+dir) {
		t.Errorf("the image must create %s, or the first score flush fails", dir)
	}
}

// The pinned path is only durable if compose mounts a volume over its directory.
// A path with no volume under it survives restarts of the same container but not
// recreation — and with PROXY_IDLE_TIMEOUT plus restart: on-failure, recreation
// is the normal cycle.
func TestComposeMountsAVolumeOverTheScorePath(t *testing.T) {
	dockerfile := repoFile(t, "Dockerfile")
	compose := repoFile(t, "docker-compose.yml")

	idx := strings.Index(dockerfile, "ENV PROXY_SCORE_FILE=")
	if idx < 0 {
		t.Skip("covered by TestDockerfilePinsAnAbsoluteScorePath")
	}
	scorePath := strings.TrimSpace(strings.SplitN(dockerfile[idx+len("ENV PROXY_SCORE_FILE="):], "\n", 2)[0])
	mountPoint := pathDir(scorePath)

	// Some named volume must be mounted at the score file's directory.
	var mounted bool
	for _, line := range strings.Split(compose, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, ":"+mountPoint) {
			mounted = true
			name := strings.TrimPrefix(strings.SplitN(line[2:], ":", 2)[0], " ")
			// A named volume, not a host bind: the latter is machine-specific
			// and would not work from the published image.
			if strings.ContainsAny(name, "./\\") {
				t.Errorf("expected a named volume at %s, got a bind mount %q", mountPoint, name)
			}
			// It must also be declared top-level, or compose refuses to start.
			if !strings.Contains(compose, "\nvolumes:\n") || !strings.Contains(compose, name+":") {
				t.Errorf("named volume %q is mounted but never declared top-level", name)
			}
		}
	}
	if !mounted {
		t.Fatalf("docker-compose.yml must mount a volume at %s, or the container "+
			"relearns every model on each recreation", mountPoint)
	}
}

// pathDir is filepath.Dir with forward slashes, since these are container paths
// and the test also runs on Windows.
func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}
