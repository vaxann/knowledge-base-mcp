package secret

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("ghp_exampletoken")
	sealed, err := box.Seal(plain, []byte("repo-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("sealed value contains the plaintext")
	}
	got, err := box.Open(sealed, []byte("repo-1"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	again, _ := box.Seal(plain, []byte("repo-1"))
	if bytes.Equal(sealed, again) {
		t.Error("two seals of the same value must differ (random nonce)")
	}
}

func TestOpenFailures(t *testing.T) {
	box, _ := New(newKey(t))
	other, _ := New(newKey(t))
	sealed, _ := box.Seal([]byte("value"), []byte("repo-1"))
	damaged := append([]byte(nil), sealed...)
	damaged[len(damaged)-1] ^= 1
	cases := []struct {
		name   string
		box    *Box
		sealed []byte
		aad    string
	}{
		{"wrong key", other, sealed, "repo-1"},
		{"wrong associated data", box, sealed, "repo-2"},
		{"damaged value", box, damaged, "repo-1"},
		{"too short", box, []byte{1, 2, 3}, "repo-1"},
		{"empty", box, nil, "repo-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.box.Open(c.sealed, []byte(c.aad)); !errors.Is(err, ErrOpen) {
				t.Errorf("err = %v, want ErrOpen", err)
			}
		})
	}
}

func TestParseKey(t *testing.T) {
	raw := newKey(t)
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"standard", base64.StdEncoding.EncodeToString(raw), true},
		{"raw standard", base64.RawStdEncoding.EncodeToString(raw), true},
		{"url", base64.URLEncoding.EncodeToString(raw), true},
		{"surrounding space", "  " + base64.StdEncoding.EncodeToString(raw) + "\n", true},
		{"empty", "", false},
		{"not base64", "!!! not a key !!!", false},
		{"too short", base64.StdEncoding.EncodeToString(raw[:16]), false},
		{"too long", base64.StdEncoding.EncodeToString(append(raw, raw...)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, err := ParseKey(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if c.ok && !bytes.Equal(key, raw) {
				t.Error("decoded key differs")
			}
		})
	}
	if _, err := New(raw[:10]); err == nil {
		t.Error("New must reject a short key")
	}
}
