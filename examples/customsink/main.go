// Command customsink restores a snapshot into something other than a
// directory. It implements ark.Sink over a zip archive, so the snapshot of a
// directory comes back as one .zip file.
//
//	go run ./customsink
package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

// zipSink writes every directory and regular file of a snapshot into a zip
// archive.
type zipSink struct {
	zw *zip.Writer
}

// Open runs once, before any entry. A sink rejects here what it cannot
// restore.
func (s *zipSink) Open(_ context.Context, meta ark.SourceMeta) error {
	if meta.Type != ark.SourceTypeFile {
		return fmt.Errorf("%w: the zip sink restores file snapshots, not %q",
			ark.ErrIncompatibleSourceType, meta.Type)
	}
	return nil
}

// Receive runs once per entry, parents before children. r is nil for
// everything but a regular file, and a regular file is read to the end.
func (s *zipSink) Receive(_ context.Context, e ark.FileInfo, r io.Reader) error {
	h := &zip.FileHeader{Name: e.Path, Modified: e.ModTime}
	switch e.Type {
	case ark.FileTypeDirectory:
		h.Name = strings.TrimSuffix(e.Path, "/") + "/"
		h.SetMode(e.Mode | fs.ModeDir)
		_, err := s.zw.CreateHeader(h)
		return err
	case ark.FileTypeRegular:
		h.Method = zip.Deflate
		h.SetMode(e.Mode)
		w, err := s.zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return err
	case ark.FileTypeSymlink, ark.FileTypeHardlink, ark.FileTypeFifo, ark.FileTypeCharDevice,
		ark.FileTypeBlockDevice, ark.FileTypeUnsupported:
		// Links, fifos and devices are left out of the archive.
	}
	return nil
}

// Close runs once after a successful Open, also after a failed Receive or a
// cancelled restore. It does not run when Open fails.
func (s *zipSink) Close() error {
	return s.zw.Close()
}

func main() {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "ark-customsink-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "site")
	if err := os.MkdirAll(filepath.Join(src, "css"), 0o755); err != nil {
		log.Fatal(err)
	}
	for name, content := range map[string]string{
		"index.html":    "<h1>hello</h1>\n",
		"css/site.css":  "h1 { color: teal }\n",
		"css/print.css": "h1 { color: black }\n",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
			log.Fatal(err)
		}
	}

	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.NoPassword()); err != nil {
		log.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.NoPassword())
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()

	snap, err := arkfs.Backup(ctx, repo, src)
	if err != nil {
		log.Fatal(err)
	}

	archive := filepath.Join(dir, "site.zip")
	f, err := os.Create(archive)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := repo.Restore(ctx, snap.ID, &zipSink{zw: zip.NewWriter(f)}); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	zr, err := zip.OpenReader(archive)
	if err != nil {
		log.Fatal(err)
	}
	defer zr.Close()
	fmt.Printf("snapshot %s restored into %s:\n", snap.ID, filepath.Base(archive))
	for _, entry := range zr.File {
		fmt.Printf("%-16s %3d bytes\n", entry.Name, entry.UncompressedSize64)
	}
}
