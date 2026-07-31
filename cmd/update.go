package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// githubAPIBase is the GitHub REST API root. Overridable in tests to point at a
// local httptest server.
var githubAPIBase = "https://api.github.com"

// updateHTTPClient is the client used for release checks and downloads. Short
// timeouts so a slow/unreachable GitHub never blocks startup or hangs `update`.
var updateHTTPClient = &http.Client{Timeout: 30 * time.Second}

// releaseInfo is the trimmed shape of the GitHub "latest release" response.
type releaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// updateStatus is the cached result of the most recent background update check,
// exposed via GET /version.
type updateStatus struct {
	Current   string `json:"version"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"update_available"`
	Checked   bool   `json:"checked"`
	Error     string `json:"error,omitempty"`
}

// lastUpdateStatus holds the latest updateStatus for /version. atomic.Value so
// the background checker can publish it without locking the request path.
var lastUpdateStatus atomic.Value // updateStatus

func currentUpdateStatus() updateStatus {
	if v, ok := lastUpdateStatus.Load().(updateStatus); ok {
		return v
	}
	return updateStatus{Current: version, Checked: false}
}

// --- semver (pure, testable) ---

// parseSemver extracts the numeric MAJOR.MINOR.PATCH from a tag like "v0.2.2"
// or "1.4". A pre-release/build suffix ("-rc1", "+meta") is ignored. Missing
// components default to 0. ok=false when no numeric component is present.
func parseSemver(tag string) (parts [3]int, ok bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return [3]int{}, false
	}
	for idx, seg := range strings.SplitN(s, ".", 3) {
		n, err := strconv.Atoi(strings.TrimSpace(seg))
		if err != nil {
			return [3]int{}, false
		}
		parts[idx] = n
	}
	return parts, true
}

// semverLess reports whether version a is strictly older than b. Unparseable
// versions (e.g. "dev") are treated as oldest, so a real release always wins.
func semverLess(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	switch {
	case !oka && !okb:
		return false
	case !oka:
		return true
	case !okb:
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// --- asset naming (pure, testable) ---

// assetNameFor returns the release-archive filename and whether it is a zip
// (Windows) vs. tar.gz (everything else), matching the naming in
// .github/workflows/release.yml exactly.
func assetNameFor(goos, goarch, tag string) (name string, isZip bool) {
	base := fmt.Sprintf("calvoproxy-%s-%s-%s", tag, goos, goarch)
	if goos == "windows" {
		return base + ".zip", true
	}
	return base + ".tar.gz", false
}

// binaryNameFor returns the executable name inside the archive.
func binaryNameFor(goos string) string {
	if goos == "windows" {
		return "calvoproxy.exe"
	}
	return "calvoproxy"
}

// --- release fetch ---

func fetchLatestRelease(ctx context.Context) (*releaseInfo, error) {
	url := strings.TrimRight(githubAPIBase, "/") + "/repos/" + repoSlug + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "calvoproxy-updater/"+version)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github releases API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release has no tag_name")
	}
	return &rel, nil
}

func (r *releaseInfo) assetURL(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, true
		}
	}
	return "", false
}

// --- checksum verification (pure, testable) ---

// sha256FromSums finds the checksum for filename in a SHA256SUMS.txt body
// (lines of "<hex>  <name>", the sha256sum format).
func sha256FromSums(sums, filename string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == filename {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func verifySHA256(data []byte, wantHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantHex)
	}
	return nil
}

// --- archive extraction (pure, testable) ---

// extractBinary pulls the named executable out of a release archive
// (zip or tar.gz) held entirely in memory.
func extractBinary(archive []byte, isZip bool, binName string) ([]byte, error) {
	if isZip {
		return extractFromZip(archive, binName)
	}
	return extractFromTarGz(archive, binName)
}

func extractFromZip(archive []byte, binName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != binName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 512<<20))
	}
	return nil, fmt.Errorf("%s not found in archive", binName)
}

func extractFromTarGz(archive []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) != binName {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, 512<<20))
	}
	return nil, fmt.Errorf("%s not found in archive", binName)
}

// --- in-place executable replacement ---

// replaceExecutable atomically swaps the running executable for newBin. On
// Unix it writes a sibling temp file and renames it over the target (atomic on
// the same filesystem). On Windows the running .exe can't be overwritten, so it
// is renamed aside to <exe>.old first; the new file then takes its place and
// the old copy is best-effort removed on next start.
func replaceExecutable(exePath string, newBin []byte) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".calvoproxy-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(newBin); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return err
	}

	if runtime.GOOS == "windows" {
		old := exePath + ".old"
		_ = os.Remove(old) // clear any leftover from a previous update
		if err := os.Rename(exePath, old); err != nil {
			cleanup()
			return fmt.Errorf("move current exe aside: %w", err)
		}
		if err := os.Rename(tmpName, exePath); err != nil {
			_ = os.Rename(old, exePath) // roll back
			return fmt.Errorf("install new exe: %w", err)
		}
		// The .old copy can't be deleted while it may still be mapped; leave it.
		return nil
	}

	if err := os.Rename(tmpName, exePath); err != nil {
		cleanup()
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// cleanupStaleUpdate removes a leftover <exe>.old from a prior Windows update.
func cleanupStaleUpdate() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// --- orchestration ---

// checkForUpdate fetches the latest release and reports whether it is newer.
func checkForUpdate(ctx context.Context) (status updateStatus, rel *releaseInfo, err error) {
	status = updateStatus{Current: version, Checked: true}
	rel, err = fetchLatestRelease(ctx)
	if err != nil {
		status.Error = err.Error()
		return status, nil, err
	}
	status.Latest = rel.TagName
	status.Available = semverLess(version, rel.TagName)
	return status, rel, nil
}

// announceUpdate runs a best-effort background check at startup and logs a
// recommendation if a newer release exists. Silent for dev builds or when
// PROXY_UPDATE_CHECK=false. The result is cached for GET /version.
func announceUpdate() {
	if isDevBuild() || strings.EqualFold(os.Getenv("PROXY_UPDATE_CHECK"), "false") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	status, rel, err := checkForUpdate(ctx)
	lastUpdateStatus.Store(status)
	if err != nil {
		slog.Debug("update check failed", "error", err)
		return
	}
	if !status.Available {
		return
	}
	if inContainer() {
		slog.Warn("A newer CalvoProxy is available — update the image",
			"current", version, "latest", rel.TagName,
			"how", "docker pull ghcr.io/"+repoSlug+":"+rel.TagName+" && recreate the container")
	} else {
		slog.Warn("A newer CalvoProxy is available — run `calvoproxy update` to upgrade",
			"current", version, "latest", rel.TagName, "release", rel.HTMLURL)
	}
}

// runUpdate implements the `calvoproxy update` subcommand: check, and if a
// newer release exists, download the matching archive, verify its checksum,
// extract the binary and swap it in place. Returns a process exit code.
func runUpdate(args []string) int {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}

	if isDevBuild() && !force {
		fmt.Println("This is a dev build (no embedded version); nothing to update. Use --force to update anyway.")
		return 0
	}
	if inContainer() {
		fmt.Println("Running inside a container — self-update isn't supported here.")
		fmt.Println("Pull a new image instead:")
		fmt.Printf("  docker pull ghcr.io/%s:latest\n", repoSlug)
		fmt.Println("  then recreate the container (docker compose up -d, or docker run …).")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("Current version: %s\nChecking %s for updates…\n", version, repoSlug)
	status, rel, err := checkForUpdate(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update check failed: %v\n", err)
		return 1
	}
	if !status.Available && !force {
		fmt.Printf("Already up to date (latest is %s).\n", rel.TagName)
		return 0
	}
	fmt.Printf("Updating %s → %s …\n", version, rel.TagName)

	name, isZip := assetNameFor(runtime.GOOS, runtime.GOARCH, rel.TagName)
	assetURL, ok := rel.assetURL(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "no release asset %q for this platform (%s/%s)\n", name, runtime.GOOS, runtime.GOARCH)
		return 1
	}

	archive, err := download(ctx, assetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download %s: %v\n", name, err)
		return 1
	}

	// Verify against SHA256SUMS.txt when the release publishes it.
	if sumsURL, ok := rel.assetURL("SHA256SUMS.txt"); ok {
		sums, err := download(ctx, sumsURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "download checksums: %v\n", err)
			return 1
		}
		want, ok := sha256FromSums(string(sums), name)
		if !ok {
			fmt.Fprintf(os.Stderr, "checksum for %s not found in SHA256SUMS.txt\n", name)
			return 1
		}
		if err := verifySHA256(archive, want); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Println("Checksum verified.")
	} else {
		fmt.Println("Warning: release has no SHA256SUMS.txt; skipping checksum verification.")
	}

	newBin, err := extractBinary(archive, isZip, binaryNameFor(runtime.GOOS))
	if err != nil {
		fmt.Fprintf(os.Stderr, "extract binary: %v\n", err)
		return 1
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate current executable: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	if err := replaceExecutable(exePath, newBin); err != nil {
		fmt.Fprintf(os.Stderr, "install update: %v\n", err)
		return 1
	}

	fmt.Printf("Updated to %s. Restart CalvoProxy to run the new version.\n", rel.TagName)
	return 0
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "calvoproxy-updater/"+version)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}
