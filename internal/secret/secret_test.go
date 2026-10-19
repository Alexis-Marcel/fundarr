package secret

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k := bytes.Repeat([]byte{7}, 32)
	b, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	s1, s2 := b.Seal([]byte("api-secret")), b.Seal([]byte("api-secret"))
	if bytes.Equal(s1, s2) {
		t.Fatal("nonce must differ")
	}
	got, err := b.Open(s1)
	if err != nil || string(got) != "api-secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(s1); err != ErrCorrupt {
		t.Fatalf("wrong key must fail, got %v", err)
	}
	if _, err := b.Open([]byte{1}); err != ErrCorrupt {
		t.Fatal("short input must fail")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key must fail")
	}
	if bytes.Equal(b.Key("a"), b.Key("b")) || len(b.Key("a")) != 32 {
		t.Fatal("derived keys")
	}
}

func TestLoadMaster(t *testing.T) {
	dir := t.TempDir()
	k1, created, err := LoadMaster("", dir)
	if err != nil || !created || len(k1) != 32 {
		t.Fatalf("create: %v %v", created, err)
	}
	st, _ := os.Stat(filepath.Join(dir, "secret.key"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	k2, created, err := LoadMaster("", dir)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatal("reload must return the same key")
	}
	env := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k3, _, err := LoadMaster(env, dir)
	if err != nil || k3[0] != 1 {
		t.Fatal("env wins")
	}
	if _, _, err := LoadMaster("bad", dir); err == nil {
		t.Fatal("bad env must fail")
	}
	os.WriteFile(filepath.Join(dir, "secret.key"), []byte("nope"), 0o600)
	if _, _, err := LoadMaster("", dir); err == nil {
		t.Fatal("bad file must fail")
	}
}
