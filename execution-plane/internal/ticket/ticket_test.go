package ticket

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyAndExpiry(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("k", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaims("account-1", "slot-1", "srv74", 9, []string{"messages", "count_tokens"}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(token, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Epoch != 9 || !verified.HasScope("messages") || verified.HasScope("admin") {
		t.Fatalf("unexpected claims: %+v", verified)
	}
	if _, err := verifier.Verify(token, now.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expired, got %v", err)
	}
}

func TestRejectsTamperingAndShortKeys(t *testing.T) {
	if _, err := NewIssuer(ed25519.PrivateKey("short")); err == nil {
		t.Fatal("expected invalid private key error")
	}
	if _, err := NewVerifier(ed25519.PublicKey("short")); err == nil {
		t.Fatal("expected invalid public key error")
	}
	publicKey, privateKey, _ := ed25519.GenerateKey(strings.NewReader(strings.Repeat("k", ed25519.SeedSize)))
	issuer, _ := NewIssuer(privateKey)
	verifier, _ := NewVerifier(publicKey)
	now := time.Unix(2_000_000_000, 0)
	claims, _ := NewClaims("account-1", "slot-1", "srv74", 1, []string{"messages"}, now, time.Minute)
	token, _ := issuer.Sign(claims)
	parts := strings.Split(token, ".")
	tampered := "A" + parts[0][1:] + "." + parts[1]
	if _, err := verifier.Verify(tampered, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected signature error, got %v", err)
	}
}
