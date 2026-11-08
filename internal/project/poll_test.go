package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSignature(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n")
	write("main.go", "package main\n")
	base := signature(dir)

	write("README.md", "irrelevant")
	write(".git/HEAD", "hidden dir")
	if signature(dir) != base {
		t.Error("irrelevant or hidden files changed the signature")
	}

	// same size, different mtime (an edit that keeps the length)
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "main.go"), later, later); err != nil {
		t.Fatal(err)
	}
	s1 := signature(dir)
	if s1 == base {
		t.Error("mtime change not detected")
	}

	write("sub/util.go", "package main\n")
	if signature(dir) == s1 {
		t.Error("new file in subdirectory not detected")
	}
}
