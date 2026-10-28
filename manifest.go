package ark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"
)

// manifestVersion3 is the only supported manifest format.
const manifestVersion3 = 3

// manifest describes a CDC snapshot stored on disk.
// Only v3 manifests are written; all other versions are rejected.
type manifest struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	ParentID   string            `json:"parent_id,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	SourceName string            `json:"source_name"`
	SourceType SourceType        `json:"source_type"`
	SourceMeta map[string]string `json:"source_meta,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Host       string            `json:"host"`
	Comment    string            `json:"comment,omitempty"`
	RootTree   string            `json:"root_tree,omitempty"`
	Stats      SnapshotStats     `json:"stats"`
}

// fileEntry is the internal representation of a file inside a snapshot.
// FileInfo is embedded so that CAS lookups can pass fe.FileInfo directly
// to sinks without a field-by-field copy. The JSON layout is identical to
// the previous flat struct because Go flattens embedded fields.
type fileEntry struct {
	FileInfo
	Chunks []chunkRef `json:"chunks,omitempty"`
}

// statsJSON is the manifest representation of SnapshotStats. The v1 format froze
// the field order and the duration as a string, so the wire format lives here
// rather than in struct tags on the public type.
type statsJSON struct {
	FileCount          int     `json:"total_files"`
	DirCount           int     `json:"total_dirs"`
	Skipped            int     `json:"skipped"`
	TotalSize          int64   `json:"total_size"`
	StoredSize         int64   `json:"stored_size"`
	NewChunksRawSize   int64   `json:"new_chunks_raw_size"`
	ChunksTotal        int     `json:"chunks_total"`
	ChunksNew          int     `json:"chunks_new"`
	ChunksReused       int     `json:"chunks_reused"`
	DeduplicationRatio float64 `json:"deduplication_ratio"`
	CompressionRatio   float64 `json:"compression_ratio"`
	Duration           string  `json:"duration"`
}

// MarshalJSON writes the manifest shape: Duration becomes the string field
// "duration".
func (s SnapshotStats) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(statsJSON{
		FileCount:          s.FileCount,
		DirCount:           s.DirCount,
		Skipped:            s.Skipped,
		TotalSize:          s.TotalSize,
		StoredSize:         s.StoredSize,
		NewChunksRawSize:   s.NewChunksRawSize,
		ChunksTotal:        s.ChunksTotal,
		ChunksNew:          s.ChunksNew,
		ChunksReused:       s.ChunksReused,
		DeduplicationRatio: s.DeduplicationRatio,
		CompressionRatio:   s.CompressionRatio,
		Duration:           s.Duration.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("manifest: failed to marshal stats: %w", err)
	}
	return data, nil
}

// UnmarshalJSON reads the manifest shape. An unparsable duration leaves the
// field zero rather than failing the whole manifest.
func (s *SnapshotStats) UnmarshalJSON(data []byte) error {
	var raw statsJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("manifest: failed to parse stats: %w", err)
	}
	d, err := time.ParseDuration(raw.Duration)
	if err != nil {
		d = 0
	}
	*s = SnapshotStats{
		FileCount:          raw.FileCount,
		DirCount:           raw.DirCount,
		Skipped:            raw.Skipped,
		TotalSize:          raw.TotalSize,
		StoredSize:         raw.StoredSize,
		NewChunksRawSize:   raw.NewChunksRawSize,
		ChunksTotal:        raw.ChunksTotal,
		ChunksNew:          raw.ChunksNew,
		ChunksReused:       raw.ChunksReused,
		DeduplicationRatio: raw.DeduplicationRatio,
		CompressionRatio:   raw.CompressionRatio,
		Duration:           d,
	}
	return nil
}

// chunkRef references a content-addressable chunk in the CAS.
type chunkRef struct {
	ID     string `json:"id"`
	Size   int    `json:"size"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

// saveManifest seals m and writes it under SnapshotFile.
func (r *Repository) saveManifest(ctx context.Context, m *manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("manifest: failed to marshal: %w", err)
	}
	return r.putSealed(ctx, Handle{Type: SnapshotFile, Name: m.ID}, data)
}

// loadManifest reads and opens a manifest. One that was changed or moved
// from another snapshot id fails with ErrCorruptManifest.
func (r *Repository) loadManifest(ctx context.Context, id string) (*manifest, error) {
	data, err := r.getSealed(ctx, Handle{Type: SnapshotFile, Name: id})
	if errors.Is(err, errSealBroken) {
		return nil, fmt.Errorf("%w: snapshot %s: %w", ErrCorruptManifest, id, err)
	}
	if err != nil {
		return nil, err // wraps ErrNotFound as appropriate
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: snapshot %s: %w", ErrCorruptManifest, id, err)
	}
	if m.Version != manifestVersion3 {
		return nil, fmt.Errorf("manifest: unsupported version %d (only v3 is supported)", m.Version)
	}
	return &m, nil
}

// readManifests returns all manifests, newest first. A manifest that does not
// load is an error: a listing must not hide a snapshot, and prune must not
// treat one it cannot read as absent and sweep its objects.
func (r *Repository) readManifests(ctx context.Context) ([]*manifest, error) {
	var manifests []*manifest
	err := r.backend.List(ctx, SnapshotFile, func(info BlobInfo) error {
		m, err := r.loadManifest(ctx, info.Name)
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", info.Name, err)
		}
		manifests = append(manifests, m)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("manifest: failed to list snapshots: %w", err)
	}
	// Sort newest first (ULID IDs are lexicographically sortable).
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].ID > manifests[j].ID })
	return manifests, nil
}

// manifestToSnapshot converts an internal manifest to the public Snapshot type.
func manifestToSnapshot(m *manifest) *Snapshot {
	return &Snapshot{
		ID:         m.ID,
		SourceName: m.SourceName,
		SourceType: m.SourceType,
		SourceMeta: maps.Clone(m.SourceMeta),
		Host:       m.Host,
		Tags:       m.Tags,
		Comment:    m.Comment,
		CreatedAt:  m.CreatedAt,
		ParentID:   m.ParentID,
		Stats:      m.Stats,
	}
}

// walkSnapshot streams the entries of a snapshot to fn, parents before
// children and hardlinks after their targets, without materialising the list.
//
// filter prunes the walk before anything is read. Hardlink entries are held
// back until the end because tree order is by name, so a link can precede its
// target and both a Sink and tar need the target first; only their metadata
// waits, never file content.
func walkSnapshot(
	ctx context.Context,
	trees *objectStore,
	m *manifest,
	filter pathFilter,
	fn func(fileEntry) error,
) error {
	if m.RootTree == "" {
		return nil
	}

	var links []fileEntry
	err := walkTreeWithPrefix(ctx, trees, m.RootTree, "", filter, func(e fileEntry) error {
		if e.Type == FileTypeHardlink {
			links = append(links, e)
			return nil
		}
		return fn(e)
	})
	if err != nil {
		return fmt.Errorf("manifest: tree walk failed: %w", err)
	}

	for _, e := range links {
		// A path filter can keep a link and drop its target. The link then
		// arrives as a regular file with the target's content.
		if !filter.keep(e.LinkDest) {
			target, err := lookupTreeEntry(ctx, trees, m.RootTree, e.LinkDest)
			if err != nil {
				return fmt.Errorf("manifest: hardlink %s: %w", e.Path, err)
			}
			e.Type = FileTypeRegular
			e.LinkDest = ""
			e.Size = target.Size
			e.Chunks = target.Chunks
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// latestSnapshotID returns the id of the newest snapshot written for
// sourceName, or an empty string when the repository holds none.
//
// The manifests are the only record of what a source last backed up; there is
// no pointer object that can drift from them. Source names are compared
// verbatim as the manifest stores them.
func (r *Repository) latestSnapshotID(ctx context.Context, sourceName string) (string, error) {
	manifests, err := r.readManifests(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range manifests {
		if m.SourceName == sourceName {
			return m.ID, nil
		}
	}
	return "", nil
}
