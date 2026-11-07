package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var tinySpec = spec{
	smallN:    300,
	medN:      4,
	medMin:    64 << 10,
	medMax:    128 << 10,
	largeN:    2,
	largeSize: 256 << 10,
}

func gen(t *testing.T, seed int64) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, err := generate(context.Background(), dir, tinySpec, seed); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sum(t *testing.T, dir string) string {
	t.Helper()
	s, _, _, err := hashTree(context.Background(), dir)
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
	_, files, n, err := hashTree(context.Background(), a)
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
	if _, _, err := generate(context.Background(), dir, tinySpec, 1); err == nil {
		t.Fatal("generate wrote into a non-empty directory")
	}
}

func TestMutateDeterministic(t *testing.T) {
	a, b := gen(t, 7), gen(t, 7)
	before := sum(t, a)
	_, filesBefore, _, err := hashTree(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{a, b} {
		if err := mutate(context.Background(), d, 7); err != nil {
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
	_, filesAfter, _, err := hashTree(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if filesAfter != filesBefore {
		t.Fatalf("files %d -> %d", filesBefore, filesAfter)
	}
}

func TestMutateRounds(t *testing.T) {
	a := gen(t, 7)
	ctx := context.Background()
	_, n0, _, err := hashTree(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var sums []string
	for range 2 {
		if err := mutate(ctx, a, 7); err != nil {
			t.Fatal(err)
		}
		s, n, _, err := hashTree(ctx, a)
		if err != nil || n != n0 {
			t.Fatalf("files %d -> %d, err = %v", n0, n, err)
		}
		sums = append(sums, s)
	}
	if sums[0] == sums[1] {
		t.Fatal("second round changed nothing")
	}
	files, err := listFiles(a)
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "m") {
			added++
		}
	}
	if added != 2 {
		t.Fatalf("%d added files after two rounds, want 2", added)
	}
}

func TestMutateRejectsWrongRoot(t *testing.T) {
	a := gen(t, 7)
	parent := filepath.Dir(a)
	ctx := context.Background()
	before, _, _, err := hashTree(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutate(ctx, parent, 7); err == nil {
		t.Fatal("mutate accepted a non-root directory")
	}
	after, _, _, err := hashTree(ctx, parent)
	if err != nil || after != before {
		t.Fatalf("tree changed, err = %v", err)
	}
}

func TestGenerateCancelCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, err := generate(ctx, dir, tinySpec, 1); err == nil {
		t.Fatal("cancelled generate succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("%d entries left behind", len(entries))
	}
}
