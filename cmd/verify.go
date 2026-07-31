package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// releasePublicKey is the base64 (std-encoded) 32-byte Ed25519 public key that
// release SHA256SUMS.txt signatures are verified against. Empty = signature
// verification is DISABLED (the SHA-256 checksum is still enforced). Fill this in
// once, after generating a keypair with `go run ./tools/gen` (the public key is
// safe to commit); the matching private key goes into the CI signing secret. It
// can also be overridden at runtime with PROXY_UPDATE_PUBKEY.
var releasePublicKey = "GZXKveslK0EYlHea9EUpcdnWo6QIXmwE9ksfhq2gups="

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
