package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := testCipher(t)
	for _, plain := range []string{"", "s", "a meta app secret", strings.Repeat("x", 4096)} {
		blob, err := c.EncryptString(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		got, err := c.DecryptString(blob)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", plain, err)
		}
		if got != plain {
			t.Errorf("round trip = %q, want %q", got, plain)
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c := testCipher(t)
	a, _ := c.EncryptString("same input")
	b, _ := c.EncryptString("same input")
	// Reusing a nonce under one key breaks GCM entirely, so identical
	// plaintext must never produce identical ciphertext.
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
}

func TestDecryptRejectsTamperedAndForeignCiphertext(t *testing.T) {
	c := testCipher(t)
	blob, _ := c.EncryptString("secret value")

	t.Run("tampered", func(t *testing.T) {
		bad := bytes.Clone(blob)
		bad[len(bad)-1] ^= 0xff
		if _, err := c.Decrypt(bad); err == nil {
			t.Fatal("Decrypt accepted a tampered ciphertext")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		if _, err := c.Decrypt(blob[:4]); err == nil {
			t.Fatal("Decrypt accepted a truncated blob")
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		other, _ := NewCipher(bytes.Repeat([]byte{0x99}, 32))
		if _, err := other.Decrypt(blob); err == nil {
			t.Fatal("Decrypt accepted a ciphertext sealed under a different key")
		}
	})
}

// TestSignVerify covers the exact shapes Meta sends in X-Hub-Signature-256.
func TestSignVerify(t *testing.T) {
	secret := []byte("meta-app-secret")
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"id":"123"}]}`)
	digest := SignHex(secret, body)

	if !VerifyHex(secret, body, digest) {
		t.Fatal("VerifyHex rejected a signature it just produced")
	}

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		digest string
	}{
		{"tampered body", secret, append(bytes.Clone(body), ' '), digest},
		{"wrong secret", []byte("not-the-secret"), body, digest},
		{"empty digest", secret, body, ""},
		{"not hex", secret, body, "zzzz"},
		{"truncated digest", secret, body, digest[:len(digest)-2]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if VerifyHex(tt.secret, tt.body, tt.digest) {
				t.Error("VerifyHex accepted an invalid signature")
			}
		})
	}
}

// A single byte of added whitespace must invalidate the signature: this is why
// the raw body is never re-marshalled anywhere in the gateway.
func TestSignatureCoversExactBytes(t *testing.T) {
	secret := []byte("s")
	compact := []byte(`{"a":1,"b":2}`)
	spaced := []byte(`{"a": 1, "b": 2}`)

	if SignHex(secret, compact) == SignHex(secret, spaced) {
		t.Fatal("whitespace-only difference produced the same signature")
	}
	if VerifyHex(secret, spaced, SignHex(secret, compact)) {
		t.Fatal("a re-marshalled body verified against the original signature")
	}
}

func TestEqualConstantTime(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"token", "token", true},
		{"token", "token ", false},
		{"token", "TOKEN", false},
		{"", "", true},
		{"a", "", false},
	}
	for _, c := range cases {
		if got := EqualConstantTime(c.a, c.b); got != c.want {
			t.Errorf("EqualConstantTime(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRandomTokensAreUniqueAndDecodable(t *testing.T) {
	seen := map[string]struct{}{}
	for range 100 {
		tok, err := RandomToken(32)
		if err != nil {
			t.Fatalf("RandomToken: %v", err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatal("RandomToken returned a duplicate")
		}
		seen[tok] = struct{}{}
		// base64url without padding keeps the token safe in a URL and header.
		if strings.ContainsAny(tok, "+/=") {
			t.Errorf("token %q contains characters that are unsafe unescaped in a URL", tok)
		}
	}
}

func TestBodyHashIsStable(t *testing.T) {
	body := []byte(`{"entry":[{"id":"1"}]}`)
	if BodyHash(body) != BodyHash(bytes.Clone(body)) {
		t.Fatal("BodyHash is not deterministic")
	}
	if BodyHash(body) == BodyHash(append(bytes.Clone(body), ' ')) {
		t.Fatal("BodyHash ignored a trailing byte")
	}
	// The dedupe key is stored in a text column and must be hex.
	if len(BodyHash(body)) != 64 {
		t.Fatalf("BodyHash length = %d, want 64 hex chars", len(BodyHash(body)))
	}
}
