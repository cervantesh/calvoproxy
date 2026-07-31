// Command gen creates an Ed25519 keypair for signing CalvoProxy releases.
//
//	go run ./tools/gen
//
// It prints the PUBLIC key (base64) — paste it into cmd/verify.go's
// releasePublicKey (safe to commit) — and the PRIVATE key (base64) — store it as
// the CI signing secret RELEASE_SIGNING_KEY and nowhere else.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen failed:", err)
		os.Exit(1)
	}
	fmt.Println("# Public key — paste into cmd/verify.go `releasePublicKey` (safe to commit):")
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	fmt.Println()
	fmt.Println("# Private key — store as GitHub secret RELEASE_SIGNING_KEY (keep secret!):")
	fmt.Println(base64.StdEncoding.EncodeToString(priv))
}
