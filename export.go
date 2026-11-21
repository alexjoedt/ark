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

	// gzipMagic opens every gzip stream. Import sniffs it to take back an
	// archive the caller compressed.
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

func (r *Repository) runExport(ctx context.Context, snapshotID SnapshotID, w io.Writer) error {
	logger := r.logger.With("component", "export", "snapshot", snapshotID)

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "exporting snapshot", "files", m.Stats.FileCount, "chunks", m.Stats.ChunksTotal)

	// Same streaming path as restore: the chunks of the upcoming files are
	// fetched while the current one is written to the archive.
	chunks, trees := r.stores()
	fetcher := r.newChunkFetcher(ctx, chunks, newChunkCache(defaultRestoreCache))
	defer fetcher.close()

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
	err = streamSnapshot(ctx, trees, fetcher, m, pathFilter{}, func(entry openEntry) error {
		fe := entry.fe
		files++
		header := &tar.Header{
			Name:       exportDataPrefix + fe.Path,
			Mode:       int64(UnixMode(fe.Mode)),
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
			if _, err := io.Copy(tw, entry.stream); err != nil {
				return fmt.Errorf("export: failed to write chunk data for %s: %w", fe.Path, err)
			}
		case FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice:
			return fmt.Errorf("export: %s is a %s, which an export does not carry yet", fe.Path, fe.Type)
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

func (r *Repository) runImport(ctx context.Context, reader io.Reader, session *packSession) (*manifest, error) {
	logger := r.logger.With("component", "import")

	rd, closeGzip, err := maybeGunzip(reader)
	if err != nil {
		return nil, err
	}
	defer closeGzip()

	tr := tar.NewReader(rd)

	var m *manifest
	cr := chunker.New(nil, r.chunker)
	w := newChunkWriter(ctx, session.chunks, r.concurrency)
	defer func() { _ = w.wait() }()
	var collectedEntries []fileEntry
	taken := sourcePaths{}
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
		case header.Name == exportMetadataName:
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("import: failed to read %s: %w", header.Name, err)
			}
			if m, err = parseArchiveMetadata(data); err != nil {
				return nil, err
			}
			m.ID = importedSnapshotID(m.ID)
			exists, err := r.backend.Exists(ctx, Handle{Type: SnapshotFile, Name: string(m.ID)})
			if err != nil {
				return nil, fmt.Errorf("import: failed to check snapshot %s: %w", m.ID, err)
			}
			if exists {
				return nil, fmt.Errorf("import: snapshot %s already exists in this repository", m.ID)
			}

		case strings.HasPrefix(header.Name, exportDataPrefix):
			fe := fileEntry{FileInfo: FileInfo{
				Path:    strings.TrimPrefix(header.Name, exportDataPrefix),
				Mode:    header.FileInfo().Mode(),
				UID:     header.Uid,
				GID:     header.Gid,
				ModTime: header.ModTime,
				Xattrs:  xattrsFromPAX(header.PAXRecords),
			}}
			switch header.Typeflag {
			case tar.TypeReg:
				fe.Type = FileTypeRegular
			case tar.TypeDir:
				fe.Type = FileTypeDirectory
			case tar.TypeSymlink:
				fe.Type, fe.LinkDest = FileTypeSymlink, header.Linkname
			case tar.TypeLink:
				fe.Type, fe.LinkDest = FileTypeHardlink, strings.TrimPrefix(header.Linkname, exportDataPrefix)
			default:
				continue
			}
			if err := taken.add(fe.FileInfo); err != nil {
				return nil, fmt.Errorf("import: %w", err)
			}
			switch fe.Type {
			case FileTypeRegular:
				chunks, err := chunkFileData(w, cr, tr)
				if err != nil {
					return nil, fmt.Errorf("import: failed to chunk file %s: %w", fe.Path, err)
				}
				fe.Size, fe.Chunks = header.Size, chunks
				stats.FileCount++
				stats.TotalSize += header.Size
			case FileTypeDirectory:
				stats.DirCount++
			case FileTypeHardlink:
				stats.FileCount++
			case FileTypeSymlink, FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice, FileTypeUnsupported:
			}
			collectedEntries = append(collectedEntries, fe)
		}
	}

	if m == nil {
		return nil, fmt.Errorf("import: archive contains no %s", exportMetadataName)
	}
	if err := w.wait(); err != nil {
		return nil, fmt.Errorf("import: failed to store chunks: %w", err)
	}
	w.stats(&stats, collectedEntries)

	// Rebuild the Merkle tree from the re-chunked entries.
	rootHash, err := buildTree(ctx, session.trees, collectedEntries, r.concurrency)
	if err != nil {
		return nil, fmt.Errorf("import: failed to build snapshot tree: %w", err)
	}
	m.RootTree = rootHash
	if err := session.finish(ctx); err != nil {
		return nil, fmt.Errorf("import: failed to store packs: %w", err)
	}

	// The chunk boundaries are this repository's, not the exporting one's, so
	// the statistics are recomputed rather than carried over.
	stats.Duration = time.Since(start)
	stats.computeRatios()
	m.Stats = storedStats(stats)

	if err := r.saveManifest(ctx, m); err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "import completed", "id", m.ID, "files", stats.FileCount)
	return m, nil
}

// maybeGunzip returns reader as it is, or wrapped in a gzip reader when the
// stream starts with the gzip magic. Export writes plain tar; a caller that
// wants it compressed wraps the stream, and Import takes it back.
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

// parseArchiveMetadata reads the archive's metadata entry, a manifest. It is
// read as strictly as a stored one: the import stores it, and a key it
// dropped on the way would be gone for every later reader.
func parseArchiveMetadata(data []byte) (*manifest, error) {
	var m manifest
	if err := decodeStrict("the archive metadata", data, &m); err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			return nil, fmt.Errorf("import: %w", err)
		}
		return nil, fmt.Errorf("import: failed to parse the archive metadata: %w", err)
	}
	if m.Version != manifestVersion {
		// Saved as is, it would be a manifest no reader loads.
		return nil, fmt.Errorf("import: %w: the archive metadata has manifest version %d, this build reads version %d",
			ErrUnsupportedFormat, m.Version, manifestVersion)
	}
	return &m, nil
}
