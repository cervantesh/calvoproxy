package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	proxyv1 "github.com/cervantesh/calvoproxy/gen/proto/proxyv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// F10: gRPC GetHealth must honour PROXY_ADMIN_TOKEN (it leaks internals).
func TestGRPCGetHealth_AdminTokenGate(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "atok")
	server := &proxyTransportGRPCServer{
		routerService: &routerServiceAdapter{
			routeRequestWithProvider: func(http.ResponseWriter, *http.Request, string, string) {},
			health:                   func() interface{} { return map[string]any{"ready": true} },
		},
	}

	// No metadata → Unauthenticated.
	if _, err := server.GetHealth(context.Background(), &proxyv1.GetHealthRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated without token, got %v", err)
	}

	// Correct Bearer token in metadata → OK.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer atok"))
	if _, err := server.GetHealth(ctx, &proxyv1.GetHealthRequest{}); err != nil {
		t.Fatalf("valid token should pass, got %v", err)
	}

	// Wrong token → Unauthenticated.
	badCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-admin-token", "nope"))
	if _, err := server.GetHealth(badCtx, &proxyv1.GetHealthRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token must be rejected, got %v", err)
	}
}

// F3: `calvoproxy update` must fail closed when a release has no SHA256SUMS.txt.
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
