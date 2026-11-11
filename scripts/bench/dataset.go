package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	perDir      = 20
	smallMin    = 1 << 10
	smallMax    = 64 << 10
	bufSize     = 64 << 10
	bandSmall   = "small"
	bandMedium  = "medium"
	bandLarge   = "large"
	extText     = ".txt"
	extRandom   = ".bin"
	changeShare = 200 // add and delete len(files)/changeShare files: 1000 of 200k
)

type spec struct {
	smallN, medN, largeN int
	medMin, medMax       int
	largeSize            int
	logSizes             bool // small files log-uniform in size instead of uniform
}

var (
	fullSpec = spec{
		smallN:    200000,
		medN:      120,
		medMin:    1 << 20,
		medMax:    50 << 20,
		largeN:    2,
		largeSize: 512 << 20,
	}
	smallSpec = spec{
		smallN:    2000,
		medN:      4,
		medMin:    1 << 20,
		medMax:    10 << 20,
		largeN:    2,
		largeSize: 16 << 20,
	}
	// filesSpec is many small files only, like a source tree: ~40k files,
	// ~600 MiB, where per-object cost dominates.
	filesSpec = spec{
		smallN:   40000,
		logSizes: true,
	}
)

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

// stream is the deterministic byte and number source for one file version.
func stream(seed int64, key string) *rand.ChaCha8 {
	return rand.NewChaCha8(sha256.Sum256(fmt.Appendf(nil, "%d/%s", seed, key)))
}

func vocabulary(seed int64) [][]byte {
	r := rand.New(stream(seed, "vocab"))
	vocab := make([][]byte, 1000)
	for i := range vocab {
		w := make([]byte, 3+r.IntN(7))
		for j := range w {
			w[j] = 'a' + byte(r.IntN(26))
		}
		vocab[i] = w
	}
	return vocab
}

func fillText(b []byte, r *rand.Rand, vocab [][]byte) {
	n := 0
	for n < len(b) {
		n += copy(b[n:], vocab[r.IntN(len(vocab))])
		if n < len(b) {
			b[n] = ' '
			if r.IntN(12) == 0 {
				b[n] = '\n'
			}
			n++
		}
	}
}

// content writes size bytes: words from the vocabulary when text, else random.
func content(
	ctx context.Context,
	w io.Writer,
	c *rand.ChaCha8,
	vocab [][]byte,
	size int64,
	text bool,
) error {
	bp := bufPool.Get().(*[]byte) //nolint:errcheck // pool only holds *[]byte
	defer bufPool.Put(bp)
	buf := *bp
	r := rand.New(c)
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := buf[:min(int64(len(buf)), size)]
		if text {
			fillText(b, r, vocab)
		} else if _, err := c.Read(b); err != nil {
			return fmt.Errorf("generating bytes: %w", err)
		}
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("writing content: %w", err)
		}
		size -= int64(len(b))
	}
	return nil
}

func writeFile(
	ctx context.Context,
	path string,
	mode int,
	c *rand.ChaCha8,
	vocab [][]byte,
	size int64,
	text bool,
) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	f, err := os.OpenFile(path, mode, 0o644)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	if err := content(ctx, f, c, vocab, size, text); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// leafDir places file i of n below band: leaves of perDir files, alternating
// depth 3 and 4, in mixed-radix directories under t3 or t4.
func leafDir(band string, i, n int) string {
	leaf := i / perDir
	depth := 3 + leaf%2
	idx := leaf / 2
	count := (n+perDir-1)/perDir/2 + 1
	base := 2
	for pow(base, depth) < count {
		base++
	}
	parts := []string{band, fmt.Sprintf("t%d", depth)}
	for range depth {
		parts = append(parts, fmt.Sprintf("d%02d", idx%base))
		idx /= base
	}
	return filepath.Join(parts...)
}

func pow(b, e int) int {
	r := 1
	for range e {
		r *= b
	}
	return r
}

type job struct {
	band string
	i, n int
	min  int
	max  int
	log  bool
}

func (s spec) jobs() []job {
	jobs := make([]job, 0, s.smallN+s.medN+s.largeN)
	for i := range s.smallN {
		jobs = append(jobs, job{bandSmall, i, s.smallN, smallMin, smallMax, s.logSizes})
	}
	for i := range s.medN {
		jobs = append(jobs, job{bandMedium, i, s.medN, s.medMin, s.medMax, false})
	}
	for i := range s.largeN {
		jobs = append(jobs, job{bandLarge, i, s.largeN, s.largeSize, s.largeSize, false})
	}
	return jobs
}

// generate writes the dataset into dir, which must be empty or absent, and
// returns the file count and total bytes. A failed or cancelled run removes
// what it wrote, so the next run starts from an empty directory.
func generate(ctx context.Context, dir string, sp spec, seed int64) (int, int64, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return 0, 0, fmt.Errorf("gen: %s is not empty", dir)
	} else if err != nil && !os.IsNotExist(err) {
		return 0, 0, fmt.Errorf("gen: reading %s: %w", dir, err)
	}
	vocab := vocabulary(seed)
	jobs := sp.jobs()
	sizes := make([]int64, len(jobs))
	err := parallel(ctx, len(jobs), func(k int) error {
		j := jobs[k]
		c := stream(seed, fmt.Sprintf("%s/%d/0", j.band, j.i))
		size := int64(j.min)
		switch {
		case j.max > j.min && j.log:
			size = int64(float64(j.min) * math.Pow(float64(j.max)/float64(j.min), rand.New(c).Float64()))
		case j.max > j.min:
			size += int64(rand.New(c).IntN(j.max - j.min + 1))
		}
		text := j.i%2 == 0
		ext := extRandom
		if text {
			ext = extText
		}
		name := filepath.Join(dir, leafDir(j.band, j.i, j.n), fmt.Sprintf("f%06d%s", j.i, ext))
		sizes[k] = size
		return writeFile(ctx, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, c, vocab, size, text)
	})
	if err != nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return 0, 0, fmt.Errorf("gen: %w", err)
	}
	var total int64
	for _, s := range sizes {
		total += s
	}
	return len(jobs), total, nil
}

// mutate applies the incremental change set in place, deterministic from
// seed and the current tree: rewrite 5% of the non-large files, delete and add
// len/200 small files, and append to 1% (at least one) of the large ones, if
// any. dir must be the dataset root. Names and content depend on a digest of the file
// list, so repeated rounds change different files instead of repeating.
func mutate(ctx context.Context, dir string, seed int64) error {
	files, err := listFiles(dir)
	if err != nil {
		return fmt.Errorf("mutate: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("mutate: %s holds no files", dir)
	}
	var cand, small, large, smallDirs []string
	seen := map[string]bool{}
	tagSum := sha256.New()
	for _, f := range files {
		_, _ = io.WriteString(tagSum, f+"\n")
		switch {
		case strings.HasPrefix(f, bandLarge+"/"):
			large = append(large, f)
			continue
		case strings.HasPrefix(f, bandSmall+"/"):
			small = append(small, f)
			if d := filepath.ToSlash(filepath.Dir(f)); !seen[d] {
				seen[d] = true
				smallDirs = append(smallDirs, d)
			}
		}
		cand = append(cand, f)
	}
	if len(small) == 0 {
		return fmt.Errorf("mutate: %s has no %s/ files, -dir must be the dataset root", dir, bandSmall)
	}
	tag := hex.EncodeToString(tagSum.Sum(nil))[:8]
	changes := max(1, len(files)/changeShare)
	rewrites := max(1, len(cand)*5/100)
	if changes >= len(small) || rewrites+changes > len(cand) {
		return fmt.Errorf("mutate: %s holds too few files to mutate", dir)
	}

	vocab := vocabulary(seed)
	r := rand.New(stream(seed, "mutate/"+tag))
	deleted := make(map[string]bool, changes)
	toDelete := make([]string, 0, changes)
	for _, i := range r.Perm(len(small))[:changes] {
		deleted[small[i]] = true
		toDelete = append(toDelete, small[i])
	}
	toRewrite := make([]string, 0, rewrites)
	for _, i := range r.Perm(len(cand)) {
		if len(toRewrite) == rewrites {
			break
		}
		if !deleted[cand[i]] {
			toRewrite = append(toRewrite, cand[i])
		}
	}

	err = parallel(ctx, len(toRewrite), func(k int) error {
		rel := toRewrite[k]
		path := filepath.Join(dir, rel)
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("rewrite: %w", err)
		}
		c := stream(seed, "rewrite/"+tag+"/"+rel)
		if err := writeFile(
			ctx,
			path,
			os.O_WRONLY|os.O_TRUNC,
			c,
			vocab,
			st.Size(),
			strings.HasSuffix(rel, extText),
		); err != nil {
			return fmt.Errorf("rewrite: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mutate: %w", err)
	}
	for _, rel := range toDelete {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			return fmt.Errorf("mutate: delete: %w", err)
		}
	}
	err = parallel(ctx, changes, func(k int) error {
		c := stream(seed, fmt.Sprintf("add/%s/%d", tag, k))
		rr := rand.New(c)
		d := smallDirs[rr.IntN(len(smallDirs))]
		size := int64(smallMin + rr.IntN(smallMax-smallMin+1))
		text := k%2 == 0
		ext := extRandom
		if text {
			ext = extText
		}
		path := filepath.Join(dir, d, fmt.Sprintf("m%s_%06d%s", tag, k, ext))
		if err := writeFile(ctx, path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, c, vocab, size, text); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mutate: %w", err)
	}
	for _, k := range r.Perm(len(large))[:min(len(large), max(1, len(large)/100))] {
		path := filepath.Join(dir, large[k])
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("mutate: append: %w", err)
		}
		c := stream(seed, "append/"+tag+"/"+large[k])
		if err := writeFile(ctx, path, os.O_WRONLY|os.O_APPEND, c, vocab, st.Size()/32, false); err != nil {
			return fmt.Errorf("mutate: append: %w", err)
		}
	}
	return nil
}

// hashTree digests relative paths, sizes and contents of every regular file.
func hashTree(ctx context.Context, dir string) (string, int, int64, error) {
	files, err := listFiles(dir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("hash: %w", err)
	}
	sums := make([][sha256.Size]byte, len(files))
	sizes := make([]int64, len(files))
	err = parallel(ctx, len(files), func(k int) error {
		f, err := os.Open(filepath.Join(dir, files[k]))
		if err != nil {
			return fmt.Errorf("hash: opening file: %w", err)
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return fmt.Errorf("hash: reading %s: %w", files[k], err)
		}
		sizes[k] = n
		h.Sum(sums[k][:0])
		return nil
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("hash: %w", err)
	}
	h := sha256.New()
	var total int64
	for k, name := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", name, sizes[k])
		h.Write(sums[k][:])
		total += sizes[k]
	}
	return hex.EncodeToString(h.Sum(nil)), len(files), total, nil
}
