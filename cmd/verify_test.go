package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func genKeys(t *testing.T) (pubB64 string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func TestVerifyReleaseSignature(t *testing.T) {
	pubB64, priv := genKeys(t)
	sums := []byte("abc123  calvoproxy-linux-amd64.tar.gz\n")
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))

	if err := verifyReleaseSignature(sums, sig, pubB64); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Tampered content must fail.
	if err := verifyReleaseSignature([]byte("tampered"), sig, pubB64); err == nil {
		t.Fatal("tampered content accepted")
	}

	// Wrong key must fail.
	otherPub, _ := genKeys(t)
	if err := verifyReleaseSignature(sums, sig, otherPub); err == nil {
		t.Fatal("signature verified against the wrong key")
	}

	// Malformed inputs.
	if err := verifyReleaseSignature(sums, "not-base64!!", pubB64); err == nil {
		t.Fatal("malformed signature accepted")
	}
	if err := verifyReleaseSignature(sums, sig, "short"); err == nil {
		t.Fatal("malformed public key accepted")
	}
}

func TestUpdatePublicKey_EnvOverride(t *testing.T) {
	if got := updatePublicKey(); got != releasePublicKey {
		t.Fatalf("default should be the embedded key, got %q", got)
	}
	t.Setenv("PROXY_UPDATE_PUBKEY", "  envkey  ")
	if got := updatePublicKey(); got != "envkey" {
		t.Fatalf("env override should win (trimmed), got %q", got)
	}
}
