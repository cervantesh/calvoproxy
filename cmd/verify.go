package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/cervantesh/calvoproxy/internal/releasekey"
)

// releasePublicKey is the base64 Ed25519 public key that release SHA256SUMS.txt
// signatures are verified against. It comes from internal/releasekey so the CI
// release tooling verifies with the exact same key the shipped binaries use —
// a release can never carry a signature these binaries would reject. Empty =
// signature verification disabled (the SHA-256 checksum is still enforced).
// Overridable at runtime with PROXY_UPDATE_PUBKEY.
var releasePublicKey = releasekey.Public

// updatePublicKey returns the configured release public key (env overrides the
// embedded default), or "" when signature verification is disabled.
func updatePublicKey() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_UPDATE_PUBKEY")); v != "" {
		return v
	}
	return strings.TrimSpace(releasePublicKey)
}

// verifyReleaseSignature checks a base64 Ed25519 detached signature over the
// exact bytes of the signed file (SHA256SUMS.txt). Because that file already
// pins every artifact's SHA-256, signing it transitively authenticates every
// release asset with one signature.
func verifyReleaseSignature(signed []byte, sigB64, pubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubB64))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid release public key (want base64 of %d bytes)", ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature encoding (want base64 of %d bytes)", ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), signed, sig) {
		return fmt.Errorf("signature does not match the release public key")
	}
	return nil
}
