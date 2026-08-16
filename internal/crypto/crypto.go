// Package crypto provides secret encryption at rest and the HMAC helpers used
// to verify inbound provider signatures and sign outbound forwarded requests.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrDecrypt is returned for any decryption failure. The cause is deliberately
// not distinguished: telling a caller whether the nonce, the ciphertext, or
// the key was wrong is a decryption oracle.
var ErrDecrypt = errors.New("decrypt: ciphertext is invalid or the encryption key has changed")

// Cipher encrypts and decrypts secrets with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte key (validated at startup by the
// config package).
func NewCipher(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext, returning nonce||ciphertext. A fresh random nonce is
// generated per call: GCM catastrophically loses confidentiality and integrity
// if a nonce is ever reused with the same key.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	// Appending to nonce puts the ciphertext directly after it in one buffer.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens a nonce||ciphertext blob produced by Encrypt.
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, ErrDecrypt
	}
	plaintext, err := c.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// EncryptString is a convenience wrapper for text secrets.
func (c *Cipher) EncryptString(s string) ([]byte, error) { return c.Encrypt([]byte(s)) }

// DecryptString is a convenience wrapper for text secrets.
func (c *Cipher) DecryptString(blob []byte) (string, error) {
	b, err := c.Decrypt(blob)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SignHex returns the hex-encoded HMAC-SHA256 of body under secret. This is the
// format Meta uses in X-Hub-Signature-256 and the format hookfan uses for
// X-Hookfan-Signature.
func SignHex(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHex reports whether digest is a valid hex HMAC-SHA256 of body under
// secret, comparing in constant time.
func VerifyHex(secret, body []byte, digest string) bool {
	want, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	// hmac.Equal is constant time, so a mismatch reveals nothing about where
	// the two digests first differ.
	return hmac.Equal(mac.Sum(nil), want)
}

// EqualConstantTime compares two strings without leaking their contents through
// timing. Used for the Meta challenge verify token.
func EqualConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomToken returns n random bytes encoded as unpadded base64url, suitable
// for link tokens and public identifiers.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RandomHex returns n random bytes hex-encoded, used for the link handshake
// challenge.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// BodyHash returns the SHA-256 of body as hex. Used as the event dedupe key and
// for logging a signature failure without ever logging the body itself.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
