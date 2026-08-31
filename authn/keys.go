package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	prefixBytes = 9
	secretBytes = 32
	keyMarker   = "dpt_"
)

type Pepper struct {
	ID    string
	Value []byte
}

type Keyring struct {
	peppers map[string][]byte
	primary string
	random  io.Reader
}

type PlaintextKey struct{ value []byte }

func (PlaintextKey) String() string { return "[REDACTED]" }

func (PlaintextKey) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

func (k PlaintextKey) Bytes() []byte { return append([]byte(nil), k.value...) }

type GeneratedKey struct {
	Prefix   string
	Hash     []byte
	PepperID string
	Secret   PlaintextKey
}

func NewKeyring(primary string, peppers []Pepper) (*Keyring, error) {
	return newKeyring(primary, peppers, rand.Reader)
}

func newKeyring(primary string, peppers []Pepper, random io.Reader) (*Keyring, error) {
	if strings.TrimSpace(primary) == "" || random == nil {
		return nil, errors.New("primary pepper and randomness source are required")
	}
	values := make(map[string][]byte, len(peppers))
	for _, pepper := range peppers {
		if strings.TrimSpace(pepper.ID) == "" || len(pepper.Value) < 32 {
			return nil, errors.New("pepper IDs are required and values must contain at least 32 bytes")
		}
		if _, duplicate := values[pepper.ID]; duplicate {
			return nil, fmt.Errorf("duplicate pepper %q", pepper.ID)
		}
		values[pepper.ID] = append([]byte(nil), pepper.Value...)
	}
	if _, exists := values[primary]; !exists {
		return nil, errors.New("primary pepper is not present")
	}
	return &Keyring{peppers: values, primary: primary, random: random}, nil
}

func (k *Keyring) Generate() (GeneratedKey, error) {
	prefixRaw := make([]byte, prefixBytes)
	secretRaw := make([]byte, secretBytes)
	if _, err := io.ReadFull(k.random, prefixRaw); err != nil {
		return GeneratedKey{}, fmt.Errorf("generate key prefix: %w", err)
	}
	if _, err := io.ReadFull(k.random, secretRaw); err != nil {
		return GeneratedKey{}, fmt.Errorf("generate key secret: %w", err)
	}
	prefix := base64.RawURLEncoding.EncodeToString(prefixRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	plaintext := []byte(keyMarker + prefix + "_" + secret)
	hash := digest(k.peppers[k.primary], plaintext)
	for index := range secretRaw {
		secretRaw[index] = 0
	}
	return GeneratedKey{Prefix: prefix, Hash: hash, PepperID: k.primary, Secret: PlaintextKey{value: plaintext}}, nil
}

func ParsePrefix(plaintext []byte) (string, error) {
	value := string(plaintext)
	if !strings.HasPrefix(value, keyMarker) {
		return "", errors.New("invalid gateway key format")
	}
	rest := strings.TrimPrefix(value, keyMarker)
	prefixLength := base64.RawURLEncoding.EncodedLen(prefixBytes)
	if len(rest) <= prefixLength || rest[prefixLength] != '_' {
		return "", errors.New("invalid gateway key format")
	}
	prefix := rest[:prefixLength]
	if decoded, err := base64.RawURLEncoding.DecodeString(prefix); err != nil || len(decoded) != prefixBytes {
		return "", errors.New("invalid gateway key prefix")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(rest[prefixLength+1:]); err != nil || len(decoded) != secretBytes {
		return "", errors.New("invalid gateway key secret")
	}
	return prefix, nil
}

func (k *Keyring) Verify(plaintext, expectedHash []byte, pepperID string) bool {
	pepper, ok := k.peppers[pepperID]
	if !ok || len(expectedHash) != sha256.Size {
		return false
	}
	actual := digest(pepper, plaintext)
	return subtle.ConstantTimeCompare(actual, expectedHash) == 1
}

func (k *Keyring) RehashRequired(pepperID string) bool { return pepperID != k.primary }

func (k *Keyring) Hash(plaintext []byte) ([]byte, string, error) {
	if _, err := ParsePrefix(plaintext); err != nil {
		return nil, "", err
	}
	return digest(k.peppers[k.primary], plaintext), k.primary, nil
}

func digest(pepper, plaintext []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write(plaintext)
	return mac.Sum(nil)
}
