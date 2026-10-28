package ark

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// The export archive is part of the v1 promise: an uncompressed tar stream,
// the metadata first, every entry under data/, readable by any tar tool
// without ark. Callers who want it compressed wrap the writer themselves.
const (
	exportMetadataName = "metadata.json"
	exportDataPrefix   = "data/"
	exportMetadataMode = 0o644

	// legacyMetadataName is what ark wrote before v1. Import still accepts it.
	legacyMetadataName = "manifest.json"

	// gzipMagic opens every gzip stream. Import sniffs it to stay compatible
	// with the archives the v1.1.0 binary compressed.
	gzipMagic = "\x1f\x8b"
)

// paxXattrPrefix is the de facto standard PAX key for an extended attribute,
// written by GNU tar and star and understood by archive/tar.
const paxXattrPrefix = "SCHILY.xattr."

func xattrsToPAX(xattrs map[string][]byte) map[string]string {
	if len(xattrs) == 0 {
		return nil
	}
	rec := make(map[string]string, len(xattrs))
	for name, value := range xattrs {
		rec[paxXattrPrefix+name] = string(value)
	}
	return rec
}

func xattrsFromPAX(records map[string]string) map[string][]byte {
	var xattrs map[string][]byte
	for key, value := range records {
		name, ok := strings.CutPrefix(key, paxXattrPrefix)
		if !ok {
			continue
		}
		if xattrs == nil {
			xattrs = make(map[string][]byte)
		}
		xattrs[name] = []byte(value)
	}
	return xattrs
}

func (r *Repository) runExport(ctx context.Context, snapshotID string, w io.Writer) error {
	logger := r.logger.With("component", "export", "snapshot", snapshotID)

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "exporting snapshot", "files", m.Stats.FileCount, "chunks", m.Stats.ChunksTotal)

	// Same streaming path as restore: chunks of the current file only.
	fetcher := r.newChunkFetcher(newChunkCache(defaultRestoreCache))

	tw := tar.NewWriter(w)

	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("export: failed to marshal manifest: %w", err)
	}
	header := &tar.Header{Name: exportMetadataName, Mode: exportMetadataMode, Size: int64(len(manifestData))}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("export: failed to write metadata header: %w", err)
	}
	if _, err := tw.Write(manifestData); err != nil {
		return fmt.Errorf("export: failed to write metadata: %w", err)
	}

	files := 0
	err = walkSnapshot(ctx, r.trees, m, pathFilter{}, func(fe fileEntry) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		files++
		header := &tar.Header{
			Name:       exportDataPrefix + fe.Path,
			Mode:       int64(fe.Mode),
			Uid:        fe.UID,
			Gid:        fe.GID,
			ModTime:    fe.ModTime,
			PAXRecords: xattrsToPAX(fe.Xattrs),
		}
		if len(header.PAXRecords) > 0 {
			header.Format = tar.FormatPAX
		}
		switch fe.Type {
		case FileTypeDirectory:
			header.Typeflag = tar.TypeDir
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("export: failed to write dir header %s: %w", fe.Path, err)
			}
		case FileTypeSymlink:
			header.Typeflag = tar.TypeSymlink
			header.Linkname = fe.LinkDest
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("export: failed to write symlink header %s: %w", fe.Path, err)
			}
		case FileTypeHardlink:
			// Linkname points at the other entry in this archive, not at a
			// path on the restoring host.
			header.Typeflag = tar.TypeLink
			header.Linkname = exportDataPrefix + fe.LinkDest
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("export: failed to write hardlink header %s: %w", fe.Path, err)
			}
		case FileTypeRegular:
			header.Typeflag = tar.TypeReg
			header.Size = fe.Size
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("export: failed to write file header %s: %w", fe.Path, err)
			}
			stream := fetcher.reader(ctx, fe.Chunks)
			defer stream.Close() // only cancels fetches still running
			if _, err := io.Copy(tw, stream); err != nil {
				return fmt.Errorf("export: failed to write chunk data for %s: %w", fe.Path, err)
			}
		case FileTypeUnsupported:
			// Never stored, so never exported.
		}
		return nil
	})
	if err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("export: failed to close tar writer: %w", err)
	}
	logger.InfoContext(ctx, "export completed", "files", files)
	return nil
}

func (r *Repository) runImport(ctx context.Context, reader io.Reader) (*manifest, error) {
	logger := r.logger.With("component", "import")

	rd, closeGzip, err := maybeGunzip(reader)
	if err != nil {
		return nil, err
	}
	defer closeGzip()

	tr := tar.NewReader(rd)

	var m *manifest
	cr := chunker.New(nil, r.chunker)
	w := newChunkWriter(ctx, r.chunks, r.concurrency)
	defer func() { _ = w.wait() }()
	var collectedEntries []fileEntry
	var stats SnapshotStats
	start := time.Now()

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("import: tar read error: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		switch {
		case header.Name == exportMetadataName || header.Name == legacyMetadataName:
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("import: failed to read %s: %w", header.Name, err)
			}
			if m, err = parseArchiveMetadata(data); err != nil {
				return nil, err
			}

		case strings.HasPrefix(header.Name, exportDataPrefix):
			path := strings.TrimPrefix(header.Name, exportDataPrefix)
			switch header.Typeflag {
			case tar.TypeReg:
				chunks, err := chunkFileData(w, cr, tr)
				if err != nil {
					return nil, fmt.Errorf("import: failed to chunk file %s: %w", path, err)
				}

				stats.FileCount++
				stats.TotalSize += header.Size

				collectedEntries = append(collectedEntries, fileEntry{
					FileInfo: FileInfo{
						Path:    path,
						Size:    header.Size,
						Mode:    header.FileInfo().Mode(),
						UID:     header.Uid,
						GID:     header.Gid,
						ModTime: header.ModTime,
						Type:    FileTypeRegular,
						Xattrs:  xattrsFromPAX(header.PAXRecords),
					},
					Chunks: chunks,
				})
			case tar.TypeDir:
				stats.DirCount++
				collectedEntries = append(collectedEntries, fileEntry{
					FileInfo: FileInfo{
						Path:    path,
						Mode:    header.FileInfo().Mode(),
						UID:     header.Uid,
						GID:     header.Gid,
						ModTime: header.ModTime,
						Type:    FileTypeDirectory,
						Xattrs:  xattrsFromPAX(header.PAXRecords),
					},
				})
			case tar.TypeSymlink:
				collectedEntries = append(collectedEntries, fileEntry{
					FileInfo: FileInfo{
						Path:     path,
						Mode:     header.FileInfo().Mode(),
						UID:      header.Uid,
						GID:      header.Gid,
						ModTime:  header.ModTime,
						Type:     FileTypeSymlink,
						LinkDest: header.Linkname,
						Xattrs:   xattrsFromPAX(header.PAXRecords),
					},
				})
			case tar.TypeLink:
				stats.FileCount++
				collectedEntries = append(collectedEntries, fileEntry{
					FileInfo: FileInfo{
						Path:     path,
						Mode:     header.FileInfo().Mode(),
						UID:      header.Uid,
						GID:      header.Gid,
						ModTime:  header.ModTime,
						Type:     FileTypeHardlink,
						LinkDest: strings.TrimPrefix(header.Linkname, exportDataPrefix),
						Xattrs:   xattrsFromPAX(header.PAXRecords),
					},
				})
			}
		}
	}

	if m == nil {
		return nil, fmt.Errorf("import: archive contains no %s", exportMetadataName)
	}
	if m.ID == "" {
		// A v1.1.0 archive may name only its volume.
		m.ID = generateID(time.Now())
	}

	if err := w.wait(); err != nil {
		return nil, fmt.Errorf("import: failed to store chunks: %w", err)
	}
	w.fill(collectedEntries)
	for _, e := range collectedEntries {
		stats.countChunks(e.Chunks)
	}

	// Rebuild the Merkle tree from the re-chunked entries.
	rootHash, err := buildTree(ctx, r.trees, collectedEntries)
	if err != nil {
		return nil, fmt.Errorf("import: failed to build snapshot tree: %w", err)
	}
	m.RootTree = rootHash

	// The chunk boundaries are this repository's, not the exporting one's, so
	// the statistics are recomputed rather than carried over.
	stats.Duration = time.Since(start)
	stats.computeRatios()
	m.Stats = stats

	if err := r.saveManifest(ctx, m); err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "import completed", "id", m.ID, "files", stats.FileCount)
	return m, nil
}

// maybeGunzip returns reader as it is, or wrapped in a gzip reader when the
// stream starts with the gzip magic. ark writes plain tar since v1, and the
// archives the v1.1.0 binary wrote are gzipped.
func maybeGunzip(reader io.Reader) (io.Reader, func(), error) {
	br := bufio.NewReader(reader)
	magic, err := br.Peek(len(gzipMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("import: failed to read the archive: %w", err)
	}
	if string(magic) != gzipMagic {
		return br, func() {}, nil
	}

	gr, err := gzip.NewReader(br)
	if err != nil {
		return nil, nil, fmt.Errorf("import: failed to create gzip reader: %w", err)
	}
	return gr, func() { _ = gr.Close() }, nil
}

// legacyMetadata is the metadata.json the v1.1.0 binary wrote. Only the fields
// that survive a re-chunking import are read; everything else about the
// snapshot is rebuilt from the archive itself.
type legacyMetadata struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Source    struct {
		Volume string            `json:"volume"`
		Labels map[string]string `json:"labels"`
	} `json:"source"`
	Backup struct {
		Comment string `json:"comment"`
	} `json:"backup"`
}

// parseArchiveMetadata reads the archive's metadata entry, in this version's
// shape or in the one the v1.1.0 binary wrote.
func parseArchiveMetadata(data []byte) (*manifest, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("import: failed to parse the archive metadata: %w", err)
	}
	if m.Version == manifestVersion3 {
		return &m, nil
	}
	if m.Version != 0 {
		// Saved as is, it would be a manifest no reader loads and prune sweeps.
		return nil, fmt.Errorf("import: unsupported manifest version %d (only v3 is supported)", m.Version)
	}

	// No version field: an archive from before the manifest was the metadata.
	var legacy legacyMetadata
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("import: failed to parse the archive metadata: %w", err)
	}
	if legacy.ID == "" && legacy.Source.Volume == "" {
		return nil, errors.New("import: the archive metadata names neither a snapshot nor a source")
	}

	return &manifest{
		Version:    manifestVersion3,
		ID:         legacy.ID,
		CreatedAt:  legacy.CreatedAt,
		SourceName: legacy.Source.Volume,
		// The v1.1.0 binary backed up Docker volumes and nothing else. The
		// value is the string arkdocker uses; source types are stored
		// verbatim, so naming it here creates no dependency on that package.
		SourceType: "docker-volume",
		SourceMeta: legacy.Source.Labels,
		Comment:    legacy.Backup.Comment,
	}, nil
}
