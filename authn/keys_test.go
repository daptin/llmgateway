package authn

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateParseVerifyAndRotate(t *testing.T) {
	oldPepper := bytes.Repeat([]byte{1}, 32)
	newPepper := bytes.Repeat([]byte{2}, 32)
	random := bytes.NewReader(bytes.Repeat([]byte{3}, prefixBytes+secretBytes))
	oldRing, err := newKeyring("old", []Pepper{{ID: "old", Value: oldPepper}}, random)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := oldRing.Generate()
	if err != nil {
		t.Fatal(err)
	}
	plaintext := generated.Secret.Bytes()
	if generated.Prefix == "" || !oldRing.Verify(plaintext, generated.Hash, generated.PepperID) {
		t.Fatal("generated key did not verify")
	}
	if prefix, err := ParsePrefix(plaintext); err != nil || prefix != generated.Prefix {
		t.Fatalf("prefix=%q err=%v", prefix, err)
	}
	rotated, err := NewKeyring("new", []Pepper{{ID: "old", Value: oldPepper}, {ID: "new", Value: newPepper}})
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.Verify(plaintext, generated.Hash, "old") || !rotated.RehashRequired("old") {
		t.Fatal("rotation did not retain previous pepper verification")
	}
	rehashed, pepperID, err := rotated.Hash(plaintext)
	if err != nil || pepperID != "new" || !rotated.Verify(plaintext, rehashed, pepperID) || rotated.RehashRequired(pepperID) {
		t.Fatalf("rehash failed: pepper=%s err=%v", pepperID, err)
	}
}

func TestPlaintextKeyNeverFormatsOrSerializes(t *testing.T) {
	secret := PlaintextKey{value: []byte("dpt_sensitive")}
	if strings.Contains(secret.String(), "sensitive") {
		t.Fatal("String leaked key")
	}
	payload, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "sensitive") {
		t.Fatalf("JSON leaked key: %s", payload)
	}
}

func TestMalformedKeysAreRejected(t *testing.T) {
	for _, value := range []string{"", "sk_test", "dpt_missing", "dpt_bad_secret", "dpt_YWJj_bad"} {
		if _, err := ParsePrefix([]byte(value)); err == nil {
			t.Fatalf("accepted malformed key %q", value)
		}
	}
}

func TestParsePrefixAllowsURLSafeUnderscore(t *testing.T) {
	pepper := bytes.Repeat([]byte{1}, 32)
	random := bytes.NewReader(append(bytes.Repeat([]byte{0xff}, prefixBytes), bytes.Repeat([]byte{3}, secretBytes)...))
	ring, err := newKeyring("primary", []Pepper{{ID: "primary", Value: pepper}}, random)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := ring.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generated.Prefix, "_") {
		t.Fatalf("fixture prefix does not exercise underscore: %q", generated.Prefix)
	}
	if prefix, err := ParsePrefix(generated.Secret.Bytes()); err != nil || prefix != generated.Prefix {
		t.Fatalf("prefix=%q err=%v", prefix, err)
	}
}
