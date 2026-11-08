package project

import (
	"log/slog"
	"os"
	"os/exec"
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

// The restart-skip logic relies on reproducible builds: the same sources must
// produce byte-identical binaries, even with different output names.
func TestBuildIsReproducible(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	m, err := NewManager(Config{Root: filepath.Join(root, "projects"), BuildDir: filepath.Join(root, "build"), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	p := newProject(m, "demo")
	var sums [2][32]byte
	for i := range sums {
		if i == 1 {
			// Becoming a git repository (with a commit) must not change the
			// binary either: builds don't stamp VCS information.
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
		}
		bin, err := p.build(t.Context(), false)
		if err != nil {
			t.Fatal(err)
		}
		if sums[i], err = fileHash(bin); err != nil {
			t.Fatal(err)
		}
	}
	if sums[0] != sums[1] {
		t.Fatal("identical sources produced different binaries")
	}
}
