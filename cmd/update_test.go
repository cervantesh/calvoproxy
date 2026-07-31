package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v0.2.2", [3]int{0, 2, 2}, true},
		{"0.2.2", [3]int{0, 2, 2}, true},
		{"v1.4", [3]int{1, 4, 0}, true},
		{"v2", [3]int{2, 0, 0}, true},
		{"v0.2.2-rc1", [3]int{0, 2, 2}, true},
		{"v0.2.2+build.5", [3]int{0, 2, 2}, true},
		{"dev", [3]int{}, false},
		{"", [3]int{}, false},
		{"v1.x.0", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSemver(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.1", "v0.2.2", true},
		{"v0.2.2", "v0.2.2", false},
		{"v0.2.2", "v0.2.1", false},
		{"v0.9.0", "v0.10.0", true},
		{"v1.0.0", "v0.99.99", false},
		{"dev", "v0.0.1", true},  // dev is oldest
		{"v0.0.1", "dev", false}, // a real release beats dev
		{"dev", "dev", false},    // neither parses
		{"v0.2.2", "garbage", false},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetNameFor(t *testing.T) {
	name, isZip := assetNameFor("windows", "amd64", "v0.2.2")
	if name != "calvoproxy-v0.2.2-windows-amd64.zip" || !isZip {
		t.Errorf("windows: got %q,%v", name, isZip)
	}
	name, isZip = assetNameFor("linux", "arm64", "v0.2.2")
	if name != "calvoproxy-v0.2.2-linux-arm64.tar.gz" || isZip {
		t.Errorf("linux: got %q,%v", name, isZip)
	}
	if binaryNameFor("windows") != "calvoproxy.exe" || binaryNameFor("linux") != "calvoproxy" {
		t.Errorf("binaryNameFor wrong")
	}
}

func TestSHA256FromSums(t *testing.T) {
	sums := "abc123  calvoproxy-v0.2.2-linux-amd64.tar.gz\n" +
		"def456  calvoproxy-v0.2.2-windows-amd64.zip\n"
	if h, ok := sha256FromSums(sums, "calvoproxy-v0.2.2-windows-amd64.zip"); !ok || h != "def456" {
		t.Errorf("got %q,%v", h, ok)
	}
	if _, ok := sha256FromSums(sums, "missing"); ok {
		t.Errorf("expected not found")
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("hello calvoproxy")
	sum := sha256.Sum256(data)
	hexed := hex.EncodeToString(sum[:])
	if err := verifySHA256(data, hexed); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := verifySHA256(data, "deadbeef"); err == nil {
		t.Errorf("bad checksum accepted")
	}
}

func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("calvoproxy-v0.2.2-windows-amd64/" + name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(content)
	zw.Close()
	return buf.Bytes()
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	tw.WriteHeader(&tar.Header{Name: "calvoproxy-v0.2.2-linux-amd64/" + name, Mode: 0o755, Size: int64(len(content))})
	tw.Write(content)
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	payload := []byte("#!binary-bytes")

	zipArchive := makeZip(t, "calvoproxy.exe", payload)
	got, err := extractBinary(zipArchive, true, "calvoproxy.exe")
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("zip extract: got %q err %v", got, err)
	}

	tgz := makeTarGz(t, "calvoproxy", payload)
	got, err = extractBinary(tgz, false, "calvoproxy")
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("targz extract: got %q err %v", got, err)
	}

	if _, err := extractBinary(zipArchive, true, "nonexistent"); err == nil {
		t.Errorf("expected error for missing binary")
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := dir + "/calvoproxy"
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(exe, []byte("new-binary")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "new-binary" {
		t.Errorf("after replace: got %q err %v", got, err)
	}
}

func TestFetchLatestReleaseAndCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/cervantesh/calvoproxy/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{
			"tag_name": "v9.9.9",
			"html_url": "https://example.test/releases/v9.9.9",
			"assets": [
				{"name": "calvoproxy-v9.9.9-linux-amd64.tar.gz", "browser_download_url": "https://example.test/a.tar.gz"},
				{"name": "SHA256SUMS.txt", "browser_download_url": "https://example.test/sums"}
			]
		}`)
	}))
	defer srv.Close()

	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = oldBase }()

	rel, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Errorf("tag: got %q", rel.TagName)
	}
	if url, ok := rel.assetURL("SHA256SUMS.txt"); !ok || url != "https://example.test/sums" {
		t.Errorf("assetURL: got %q,%v", url, ok)
	}

	status, _, err := checkForUpdate(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !status.Available || status.Latest != "v9.9.9" {
		t.Errorf("status: %+v (version=%q)", status, version)
	}
}

func TestFetchLatestReleaseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = oldBase }()

	if _, err := fetchLatestRelease(context.Background()); err == nil {
		t.Errorf("expected error on 404")
	}
}
