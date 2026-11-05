package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareTempDir(t *testing.T) {
	base := t.TempDir()
	dir, err := prepareTempDir(base)
	if err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, "up-123")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A restart empties a directory it created.
	if dir2, err := prepareTempDir(base); err != nil || dir2 != dir {
		t.Fatalf("restart: %q %v", dir2, err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Error("leftover file not purged")
	}
}

func TestPrepareTempDirRefusesForeignDir(t *testing.T) {
	base := t.TempDir()
	dir, err := prepareTempDir(base)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else's directory with the same name (no marker) is left alone.
	if err := os.Remove(filepath.Join(dir, tempMarker)); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "precious")
	os.WriteFile(keep, []byte("x"), 0o600)
	if _, err := prepareTempDir(base); err == nil {
		t.Fatal("want error for a directory without the marker")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("foreign file was deleted")
	}
	if _, err := prepareTempDir("relative/path"); err == nil {
		t.Error("want error for a relative base")
	}
}
