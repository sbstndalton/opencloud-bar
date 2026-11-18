package main

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func TestCapitalizeMultibyte(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "Abc", "élan": "Élan", "“quoted”": "“quoted”"} {
		got := capitalize(in)
		if got != want || !utf8.ValidString(got) {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMoveFileKeepsContentAndTime(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub", "b.txt")
	if err := os.WriteFile(from, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(from, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatal("source still there")
	}
}
