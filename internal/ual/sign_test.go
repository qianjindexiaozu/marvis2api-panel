package ual

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"testing"
)

func TestSignUsesInjectedKeyWithoutChangingBody(t *testing.T) {
	body := make([]byte, 2, 64)
	copy(body, "{}")
	before := append([]byte(nil), body[:cap(body)]...)
	got, err := Sign(body, "123", "456", "test-key")
	want := md5.Sum([]byte("{}123test-key456"))
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("signature mismatch: %v", err)
	}
	if !bytes.Equal(before, body[:cap(body)]) {
		t.Fatal("signing must not change the body or its backing array")
	}
	other, err := Sign(body, "123", "456", "other-test-key")
	if err != nil || other == got {
		t.Fatal("signature must depend on the injected key")
	}
}

func TestSignRejectsMissingKey(t *testing.T) {
	for _, key := range []string{"", " \n"} {
		if sig, err := Sign(nil, "123", "456", key); !errors.Is(err, ErrNoAccessKey) || sig != "" {
			t.Fatal("missing key must fail without producing a signature")
		}
	}
}
