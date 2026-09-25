package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodec(t *testing.T) {
	cur, prev := bytes.Repeat([]byte("c"), 32), bytes.Repeat([]byte("p"), 32)
	c, err := NewCodec(cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := c.Seal("session", []byte(`{"sub":"u"}`))

	t.Run("roundtrip", func(t *testing.T) {
		got, err := c.Open("session", tok)
		if err != nil || string(got) != `{"sub":"u"}` {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("kind mismatch", func(t *testing.T) {
		if _, err := c.Open("flow", tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tampered payload", func(t *testing.T) {
		bad := "x" + tok[1:]
		if _, err := c.Open("session", bad); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tampered signature", func(t *testing.T) {
		bad := tok[:len(tok)-1] + "A"
		if bad == tok {
			bad = tok[:len(tok)-1] + "B"
		}
		if _, err := c.Open("session", bad); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no separator", func(t *testing.T) {
		if _, err := c.Open("session", "abc"); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("previous key accepted after rotation", func(t *testing.T) {
		old, _ := NewCodec(prev, nil)
		rotated, _ := NewCodec(cur, prev)
		if _, err := rotated.Open("session", old.Seal("session", []byte("x"))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rotated codec seals with current key", func(t *testing.T) {
		rotated, _ := NewCodec(cur, prev)
		if _, err := c.Open("session", rotated.Seal("session", []byte("x"))); err != nil {
			t.Fatalf("current-key codec rejects rotated token: %v", err)
		}
	})
	t.Run("unknown key rejected", func(t *testing.T) {
		other, _ := NewCodec(prev, nil)
		if _, err := c.Open("session", other.Seal("session", []byte("x"))); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("oversize rejected", func(t *testing.T) {
		if _, err := c.Open("session", strings.Repeat("a", 5000)); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestNewCodecShortKey(t *testing.T) {
	if _, err := NewCodec(make([]byte, 31), nil); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("err = %v", err)
	}
	if _, err := NewCodec(make([]byte, 32), make([]byte, 8)); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short previous key: err = %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("  "+strings.Repeat("k", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKey(good)
	if err != nil || len(key) != 32 {
		t.Fatalf("LoadKey = %d bytes, %v", len(key), err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte(strings.Repeat("k", 31)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(short); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("err = %v", err)
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func FuzzCodecOpen(f *testing.F) {
	c, err := NewCodec(bytes.Repeat([]byte("k"), 32), nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(c.Seal("session", []byte("x")))
	f.Add("")
	f.Add(".")
	f.Fuzz(func(_ *testing.T, tok string) { _, _ = c.Open("session", tok) })
}
