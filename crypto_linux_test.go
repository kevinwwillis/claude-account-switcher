package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

// encryptV10 is Chromium's Linux encryption without a keyring.
func encryptV10(t *testing.T, plain []byte) []byte {
	t.Helper()
	key, err := linuxKey([]byte("peanuts"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	in := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(in))
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(out, in)
	return append([]byte("v10"), out...)
}

func TestDecryptV10(t *testing.T) {
	for _, plain := range []string{"", "x", "exactly sixteen!", `{"acct:synthetic":{"token":"abc"}}`} {
		got, err := decryptValue(encryptV10(t, []byte(plain)))
		if err != nil || string(got) != plain {
			t.Errorf("decrypt(%q) = %q, %v", plain, got, err)
		}
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("v10"), []byte("v12" + string(make([]byte, 32))), append([]byte("v10"), make([]byte, 15)...)} {
		if _, err := decryptValue(data); err == nil {
			t.Errorf("decrypt(%q) accepted", data)
		}
	}
}
