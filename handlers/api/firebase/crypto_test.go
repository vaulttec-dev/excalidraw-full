package firebase

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func roomKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := roomKey(t)
	elements := []byte(`[{"id":"a","type":"rectangle","version":1}]`)

	fields, err := EncryptElements(key, elements, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptElements(key, fields)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, elements) {
		t.Fatalf("decrypted %s, want %s", got, elements)
	}

	if _, err := DecryptElements(roomKey(t), fields); err == nil {
		t.Fatal("decrypted with another room's key")
	}
}

func TestSealUsesFreshIVs(t *testing.T) {
	key := roomKey(t)
	_, iv1, err := Seal(key, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, iv2, _ := Seal(key, []byte("x"))
	if len(iv1) != 12 || bytes.Equal(iv1, iv2) {
		t.Fatalf("IVs %x and %x", iv1, iv2)
	}
}

func TestRoomCipherRejectsBadKeys(t *testing.T) {
	for _, key := range []string{"", "not base64!", "c2hvcnQ"} {
		if _, err := roomCipher(key); err == nil {
			t.Fatalf("accepted key %q", key)
		}
	}
}
