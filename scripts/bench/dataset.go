package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
}

var (
	fullSpec  = spec{smallN: 200000, medN: 120, medMin: 1 << 20, medMax: 50 << 20, largeN: 2, largeSize: 512 << 20}
	smallSpec = spec{smallN: 2000, medN: 4, medMin: 1 << 20, medMax: 10 << 20, largeN: 2, largeSize: 16 << 20}
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
func content(w io.Writer, c *rand.ChaCha8, vocab [][]byte, size int64, text bool) error {
	bp := bufPool.Get().(*[]byte) //nolint:errcheck // pool only holds *[]byte
	defer bufPool.Put(bp)
	buf := *bp
	r := rand.New(c)
	for size > 0 {
		b := buf[:min(int64(len(buf)), size)]
		if text {
			fillText(b, r, vocab)
		} else if _, err := c.Read(b); err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		size -= int64(len(b))
	}
	return nil
}

func writeFile(path string, mode int, c *rand.ChaCha8, vocab [][]byte, size int64, text bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, mode, 0o644)
	if err != nil {
		return err
	}
	if err := content(f, c, vocab, size, text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
}

func (s spec) jobs() []job {
	jobs := make([]job, 0, s.smallN+s.medN+s.largeN)
	for i := range s.smallN {
		jobs = append(jobs, job{bandSmall, i, s.smallN, smallMin, smallMax})
	}
	for i := range s.medN {
		jobs = append(jobs, job{bandMedium, i, s.medN, s.medMin, s.medMax})
	}
	for i := range s.largeN {
		jobs = append(jobs, job{bandLarge, i, s.largeN, s.largeSize, s.largeSize})
	}
	return jobs
}

// generate writes the dataset into dir, which must be empty or absent, and
// returns the file count and total bytes.
func generate(dir string, sp spec, seed int64) (int, int64, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return 0, 0, fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	vocab := vocabulary(seed)
	jobs := sp.jobs()
	sizes := make([]int64, len(jobs))
	err := parallel(len(jobs), func(k int) error {
		j := jobs[k]
		c := stream(seed, fmt.Sprintf("%s/%d/0", j.band, j.i))
		size := int64(j.min)
		if j.max > j.min {
			size += int64(rand.New(c).IntN(j.max - j.min + 1))
		}
		text := j.i%2 == 0
		ext := extRandom
		if text {
			ext = extText
		}
		name := filepath.Join(dir, leafDir(j.band, j.i, j.n), fmt.Sprintf("f%06d%s", j.i, ext))
		sizes[k] = size
		return writeFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, c, vocab, size, text)
	})
	var total int64
	for _, s := range sizes {
		total += s
	}
	return len(jobs), total, err
}

// mutate applies the incremental change set in place, deterministic from
// seed: rewrite 5% of the non-large files, delete and add len/200 files, and
// append to 1% (at least one) of the large ones.
func mutate(dir string, seed int64) error {
	files, err := listFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s holds no files", dir)
	}
	var cand, large []string
	var smallDirs []string
	seen := map[string]bool{}
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, bandLarge+"/"):
			large = append(large, f)
			continue
		case strings.HasPrefix(f, bandSmall+"/"):
			if d := filepath.ToSlash(filepath.Dir(f)); !seen[d] {
				seen[d] = true
				smallDirs = append(smallDirs, d)
			}
		}
		cand = append(cand, f)
	}
	vocab := vocabulary(seed)
	r := rand.New(stream(seed, "mutate"))
	order := r.Perm(len(cand))
	changes := max(1, len(files)/changeShare)
	rewrites := max(1, len(cand)*5/100)
	if rewrites+changes > len(cand) {
		return fmt.Errorf("%s holds too few files to mutate", dir)
	}

	err = parallel(rewrites, func(k int) error {
		rel := cand[order[k]]
		path := filepath.Join(dir, rel)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		c := stream(seed, "rewrite/"+rel)
		return writeFile(path, os.O_WRONLY|os.O_TRUNC, c, vocab, st.Size(), strings.HasSuffix(rel, extText))
	})
	if err != nil {
		return err
	}
	for _, k := range order[rewrites : rewrites+changes] {
		if err := os.Remove(filepath.Join(dir, cand[k])); err != nil {
			return err
		}
	}
	err = parallel(changes, func(k int) error {
		c := stream(seed, fmt.Sprintf("add/%d", k))
		rr := rand.New(c)
		d := smallDirs[rr.IntN(len(smallDirs))]
		size := int64(smallMin + rr.IntN(smallMax-smallMin+1))
		text := k%2 == 0
		ext := extRandom
		if text {
			ext = extText
		}
		return writeFile(filepath.Join(dir, d, fmt.Sprintf("m%06d%s", k, ext)),
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC, c, vocab, size, text)
	})
	if err != nil {
		return err
	}
	for _, k := range r.Perm(len(large))[:min(len(large), max(1, len(large)/100))] {
		path := filepath.Join(dir, large[k])
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		c := stream(seed, "append/"+large[k])
		if err := writeFile(path, os.O_WRONLY|os.O_APPEND, c, vocab, st.Size()/32, false); err != nil {
			return err
		}
	}
	return nil
}

// hashTree digests relative paths, sizes and contents of every regular file.
func hashTree(dir string) (string, int, int64, error) {
	files, err := listFiles(dir)
	if err != nil {
		return "", 0, 0, err
	}
	sums := make([][sha256.Size]byte, len(files))
	sizes := make([]int64, len(files))
	err = parallel(len(files), func(k int) error {
		f, err := os.Open(filepath.Join(dir, files[k]))
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		sizes[k] = n
		h.Sum(sums[k][:0])
		return nil
	})
	if err != nil {
		return "", 0, 0, err
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
