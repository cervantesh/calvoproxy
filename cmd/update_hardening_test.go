package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// F3: `calvoproxy update` must fail closed when a release has no SHA256SUMS.txt.
//
// This test used to live in cmd/hardening_review_test.go, whose other cases
// covered the gRPC transport and went with it when that transport was removed.
// The update path did not go anywhere, so neither does its check.
func TestRunUpdate_FailsClosedWithoutChecksums(t *testing.T) {
	oldVersion, oldBase := version, githubAPIBase
	defer func() { version, githubAPIBase = oldVersion, oldBase }()
	version = "v0.0.1" // not a dev build, so update proceeds

	assetName, _ := assetNameFor(runtime.GOOS, runtime.GOARCH, "v9.9.9")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/cervantesh/calvoproxy/releases/latest":
			// Release with the platform asset but NO SHA256SUMS.txt.
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","html_url":"x","assets":[{"name":%q,"browser_download_url":%q}]}`,
				assetName, srv.URL+"/asset")
		case r.URL.Path == "/asset":
			w.Write([]byte("some-archive-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	if code := runUpdate([]string{"--force"}); code == 0 {
		t.Fatal("update must fail closed (non-zero) when SHA256SUMS.txt is absent")
	}
}
