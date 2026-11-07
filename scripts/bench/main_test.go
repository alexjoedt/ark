package main

import (
	"os"
	"path/filepath"
	"testing"
)

var tinySpec = spec{smallN: 300, medN: 4, medMin: 64 << 10, medMax: 128 << 10, largeN: 2, largeSize: 256 << 10}

func gen(t *testing.T, seed int64) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, err := generate(dir, tinySpec, seed); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sum(t *testing.T, dir string) string {
	t.Helper()
	s, _, _, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGenerateDeterministic(t *testing.T) {
	a, b, c := gen(t, 7), gen(t, 7), gen(t, 8)
	if sum(t, a) != sum(t, b) {
		t.Fatal("same seed, different trees")
	}
	if sum(t, a) == sum(t, c) {
		t.Fatal("different seed, same tree")
	}
	_, files, n, err := hashTree(a)
	if err != nil || files != 306 {
		t.Fatalf("files = %d, err = %v", files, err)
	}
	if n < 2*256<<10 {
		t.Fatalf("total %d too small", n)
	}
}

func TestGenerateRefusesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generate(dir, tinySpec, 1); err == nil {
		t.Fatal("generate wrote into a non-empty directory")
	}
}

func TestMutateDeterministic(t *testing.T) {
	a, b := gen(t, 7), gen(t, 7)
	before := sum(t, a)
	_, filesBefore, _, _ := hashTree(a)
	for _, d := range []string{a, b} {
		if err := mutate(d, 7); err != nil {
			t.Fatal(err)
		}
	}
	after := sum(t, a)
	if after == before {
		t.Fatal("mutate changed nothing")
	}
	if after != sum(t, b) {
		t.Fatal("same seed, different mutation")
	}
	// 306 files: 1 added and 1 deleted per 200 files, so the count is unchanged
	if _, filesAfter, _, _ := hashTree(a); filesAfter != filesBefore {
		t.Fatalf("files %d -> %d", filesBefore, filesAfter)
	}
}
