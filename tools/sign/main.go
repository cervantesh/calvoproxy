// Command sign produces an Ed25519 detached signature over a file, used by CI to
// sign a release's SHA256SUMS.txt.
//
//	RELEASE_SIGNING_KEY=<base64 private key> go run ./tools/sign SHA256SUMS.txt > SHA256SUMS.txt.sig
//
// The signature is written to stdout as base64. The private key is read from the
// RELEASE_SIGNING_KEY env var (base64 of the 64-byte Ed25519 private key from
// `go run ./tools/gen`), never a file or an argument.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sign <file>  (RELEASE_SIGNING_KEY env required)")
		os.Exit(2)
	}
	keyB64 := strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY"))
	if keyB64 == "" {
		fmt.Fprintln(os.Stderr, "RELEASE_SIGNING_KEY is not set")
		os.Exit(1)
	}
	priv, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		fmt.Fprintf(os.Stderr, "invalid RELEASE_SIGNING_KEY (want base64 of %d bytes)\n", ed25519.PrivateKeySize)
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read file:", err)
		os.Exit(1)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(priv), data)
	fmt.Println(base64.StdEncoding.EncodeToString(sig))
}
