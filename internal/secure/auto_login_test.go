package secure

import (
	"bytes"
	"strings"
	"testing"
)

func TestAccountAutoLoginCredentialsBoundToAccountAndPurpose(t *testing.T) {
	cipher, err := NewCipher(bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	want := AccountAutoLoginCredentials{Password: " password with spaces ", CodeURL: "https://codes.example.test/read?key=fixture-secret"}
	first, err := cipher.EncryptAccountAutoLogin("owner@icloud.com", want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cipher.EncryptAccountAutoLogin("owner@icloud.com", want)
	if err != nil || first == second || strings.Contains(first, want.Password) || strings.Contains(first, want.CodeURL) {
		t.Fatal("credential encryption must use random nonces and hide plaintext")
	}
	got, err := cipher.DecryptAccountAutoLogin("OWNER@icloud.com", first)
	if err != nil || got != want {
		t.Fatal("credential round trip changed opaque password")
	}
	if _, err := cipher.DecryptAccountAutoLogin("other@icloud.com", first); err == nil {
		t.Fatal("ciphertext was accepted for another account")
	}
	if _, err := cipher.Decrypt(first); err == nil {
		t.Fatal("IMAP decrypt accepted automatic login ciphertext")
	}
	if _, err := cipher.DecryptAppleSession(first); err == nil {
		t.Fatal("session decrypt accepted automatic login ciphertext")
	}
	other, _ := NewCipher(bytes.Repeat([]byte{18}, 32))
	if _, err := other.DecryptAccountAutoLogin("owner@icloud.com", first); err == nil {
		t.Fatal("another key decrypted automatic login credentials")
	}
	if _, err := cipher.DecryptAccountAutoLogin("owner@icloud.com", first[:len(first)-3]+"???"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
