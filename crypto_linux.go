package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"errors"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

// On Linux the app encrypts with Chromium's scheme. Values prefixed "v11"
// use a password that Chromium keeps in the desktop's keyring ("Claude Safe
// Storage", found through the Secret Service), values prefixed "v10" the
// fixed password "peanuts" that Chromium uses without a keyring. Either is
// stretched with PBKDF2 (one round) and used for AES-128-CBC.

const (
	secretSchema = "chrome_libsecret_os_crypt_password_v2"
	secretApp    = "Claude"
)

var (
	keyMu  sync.Mutex
	keyVal []byte
)

// appKey reads the key once it can. A failure, such as a keyring still
// locked after login, is tried again on the next call.
func appKey() ([]byte, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	if keyVal != nil {
		return keyVal, nil
	}
	pw, err := keyringPassword()
	if err != nil {
		return nil, err
	}
	key, err := linuxKey(pw)
	if err == nil {
		keyVal = key
	}
	return key, err
}

func linuxKey(password []byte) ([]byte, error) {
	return pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), 1, 16)
}

// keyringPassword reads the app's password from the Secret Service. It does
// not unlock a locked keyring; GNOME unlocks it at login.
func keyringPassword() ([]byte, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	svc := conn.Object("org.freedesktop.secrets", "/org/freedesktop/secrets")
	var unlocked, locked []dbus.ObjectPath
	for _, schema := range []string{secretSchema, "chrome_libsecret_os_crypt_password_v1"} {
		attrs := map[string]string{"xdg:schema": schema, "application": secretApp}
		if err := svc.Call("org.freedesktop.Secret.Service.SearchItems", 0, attrs).Store(&unlocked, &locked); err != nil {
			return nil, fmt.Errorf("search keyring: %w", err)
		}
		if len(unlocked) > 0 {
			break
		}
	}
	if len(unlocked) == 0 {
		if len(locked) > 0 {
			return nil, errors.New("the keyring holding \"Claude Safe Storage\" is locked")
		}
		return nil, errors.New("no \"Claude Safe Storage\" in the keyring")
	}
	var ignored dbus.Variant
	var session dbus.ObjectPath
	if err := svc.Call("org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&ignored, &session); err != nil {
		return nil, fmt.Errorf("open keyring session: %w", err)
	}
	defer conn.Object("org.freedesktop.secrets", session).Call("org.freedesktop.Secret.Session.Close", 0)
	var secret struct {
		Session     dbus.ObjectPath
		Parameters  []byte
		Value       []byte
		ContentType string
	}
	if err := conn.Object("org.freedesktop.secrets", unlocked[0]).Call("org.freedesktop.Secret.Item.GetSecret", 0, session).Store(&secret); err != nil {
		return nil, fmt.Errorf("read keyring item: %w", err)
	}
	return secret.Value, nil
}

func decryptValue(data []byte) ([]byte, error) {
	if len(data) < 3+aes.BlockSize {
		return nil, errors.New("unexpected encrypted value format")
	}
	var key []byte
	var err error
	switch string(data[:3]) {
	case "v11":
		key, err = appKey()
	case "v10":
		key, err = linuxKey([]byte("peanuts"))
	default:
		return nil, errors.New("unexpected encrypted value format")
	}
	if err != nil {
		return nil, err
	}
	return decryptCBC(key, data[3:])
}

func decryptCBC(key, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("bad ciphertext length")
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("bad padding")
	}
	return plain[:len(plain)-pad], nil
}
