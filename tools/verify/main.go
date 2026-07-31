// Command verify checks a detached Ed25519 signature against the SAME embedded
// release public key the shipped binaries use (internal/releasekey), so CI can
// prove — before publishing — that `calvoproxy update` will accept the release.
//
//	go run ./tools/verify dist/SHA256SUMS.txt dist/SHA256SUMS.txt.sig
//
// Exits non-zero if the key is unset, the signature is missing/malformed, or it
// does not verify.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/cervantesh/calvoproxy/internal/releasekey"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: verify <signed-file> <signature-file>")
		os.Exit(2)
	}
	pubB64 := strings.TrimSpace(releasekey.Public)
	if pubB64 == "" {
		fmt.Fprintln(os.Stderr, "internal/releasekey.Public is empty — nothing to verify against")
		os.Exit(1)
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		fmt.Fprintf(os.Stderr, "invalid embedded public key (want base64 of %d bytes)\n", ed25519.PublicKeySize)
		os.Exit(1)
	}
	signed, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read signed file:", err)
		os.Exit(1)
	}
	sigRaw, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read signature:", err)
		os.Exit(1)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		fmt.Fprintf(os.Stderr, "invalid signature encoding (want base64 of %d bytes)\n", ed25519.SignatureSize)
		os.Exit(1)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), signed, sig) {
		fmt.Fprintln(os.Stderr, "SIGNATURE DOES NOT VERIFY against the embedded release key — shipped binaries would refuse this release")
		os.Exit(1)
	}
	fmt.Println("Signature verifies against the embedded release key.")
}
