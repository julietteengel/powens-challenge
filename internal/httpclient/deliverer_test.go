package httpclient

import "testing"

// Vector independently verified via `openssl dgst -sha256 -hmac key` and
// Python's hmac module, not computed by the code under test.
func TestSign_KnownVector(t *testing.T) {
	payload := []byte("The quick brown fox jumps over the lazy dog")
	secret := []byte("key")
	want := "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"

	if got := sign(payload, secret); got != want {
		t.Errorf("sign() = %q, want %q", got, want)
	}
}
