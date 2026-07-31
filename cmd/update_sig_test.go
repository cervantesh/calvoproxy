package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// sigTestRelease serves a release whose archive matches SHA256SUMS.txt (so the
// checksum step passes and execution reaches the signature step). withSig lets a
// case attach a signature asset with arbitrary (possibly invalid) content.
func sigTestRelease(t *testing.T, archive []byte, sigContent string, withSig bool) *httptest.Server {
	t.Helper()
	assetName, _ := assetNameFor(runtime.GOOS, runtime.GOARCH, "v9.9.9")
	sum := sha256.Sum256(archive)
	sums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/cervantesh/calvoproxy/releases/latest":
			assets := fmt.Sprintf(`{"name":%q,"browser_download_url":%q},{"name":"SHA256SUMS.txt","browser_download_url":%q}`,
				assetName, srv.URL+"/archive", srv.URL+"/sums")
			if withSig {
				assets += fmt.Sprintf(`,{"name":"SHA256SUMS.txt.sig","browser_download_url":%q}`, srv.URL+"/sig")
			}
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","html_url":"x","assets":[%s]}`, assets)
		case "/archive":
			w.Write(archive)
		case "/sums":
			fmt.Fprint(w, sums)
		case "/sig":
			fmt.Fprint(w, sigContent)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func TestRunUpdate_SignatureFailClosed(t *testing.T) {
	oldVersion, oldBase := version, githubAPIBase
	defer func() { version, githubAPIBase = oldVersion, oldBase }()
	version = "v0.0.1"

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_UPDATE_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	archive := []byte("not-a-real-archive-but-checksummed")

	t.Run("missing signature refuses", func(t *testing.T) {
		srv := sigTestRelease(t, archive, "", false)
		defer srv.Close()
		githubAPIBase = srv.URL
		if code := runUpdate([]string{"--force"}); code == 0 {
			t.Fatal("must refuse when a key is configured but no SHA256SUMS.txt.sig exists")
		}
	})

	t.Run("missing signature refuses even with --insecure", func(t *testing.T) {
		srv := sigTestRelease(t, archive, "", false)
		defer srv.Close()
		githubAPIBase = srv.URL
		if code := runUpdate([]string{"--force", "--insecure"}); code == 0 {
			t.Fatal("--insecure must NOT bypass a configured signature requirement")
		}
	})

	t.Run("invalid signature refuses", func(t *testing.T) {
		badSig := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) // all-zero sig
		srv := sigTestRelease(t, archive, badSig, true)
		defer srv.Close()
		githubAPIBase = srv.URL
		if code := runUpdate([]string{"--force"}); code == 0 {
			t.Fatal("must refuse an invalid signature")
		}
	})
}
