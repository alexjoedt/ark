package out

import (
	"strings"
	"time"

	"github.com/alexjoedt/ark"
)

// Snapshot is the JSON shape of a snapshot, frozen for 1.x. The
// library struct is mapped here so its field names can move without
// breaking scripts.
type Snapshot struct {
	ID         ark.SnapshotID    `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	SourceName string            `json:"source_name"`
	SourceType string            `json:"source_type"`
	Host       string            `json:"host"`
	Tags       []string          `json:"tags"`
	Comment    string            `json:"comment"`
	Excludes   []string          `json:"excludes"`
	Labels     map[string]string `json:"labels"`
	ParentID   ark.SnapshotID    `json:"parent_id"`
	Stats      ark.SnapshotStats `json:"stats"`
}

// NewSnapshot maps s. Tags, Excludes and Labels are never null.
func NewSnapshot(s *ark.Snapshot) Snapshot {
	return Snapshot{
		ID:         s.ID,
		CreatedAt:  s.CreatedAt,
		SourceName: s.SourceName,
		SourceType: string(s.SourceType),
		Host:       s.Host,
		Tags:       orEmpty(s.Tags),
		Comment:    s.Comment,
		Excludes:   orEmpty(s.Excludes),
		Labels:     Labels(s.Labels),
		ParentID:   s.ParentID,
		Stats:      s.Stats,
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Labels is l, or an empty map for nil, so JSON never carries null.
func Labels(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}

// Snapshots maps a list. The result is never null.
func Snapshots(list []*ark.Snapshot) []Snapshot {
	res := make([]Snapshot, 0, len(list))
	for _, s := range list {
		res = append(res, NewSnapshot(s))
	}
	return res
}

// Kept is the JSON shape of a snapshot forget keeps: the snapshot and the
// rules that keep it.
type Kept struct {
	Snapshot
	Reasons []string `json:"reasons"`
}

// KeptSnapshots maps a keep list with the reasons per snapshot ID. Neither
// the result nor a Reasons list is null.
func KeptSnapshots(list []*ark.Snapshot, reasons map[ark.SnapshotID][]string) []Kept {
	res := make([]Kept, 0, len(list))
	for _, s := range list {
		res = append(res, Kept{Snapshot: NewSnapshot(s), Reasons: orEmpty(reasons[s.ID])})
	}
	return res
}

// BackupRows are the summary rows of a backup: the snapshot, its files, the
// source size, and the data new after deduplication with what it took stored.
func BackupRows(snap *ark.Snapshot) []Row {
	st := snap.Stats
	files := Count(st.FileCount)
	var notes []string
	if st.FilesUnchanged > 0 {
		notes = append(notes, Count(st.FilesUnchanged)+" unchanged")
	}
	if st.Skipped > 0 {
		notes = append(notes, Count(st.Skipped)+" skipped")
	}
	if len(notes) > 0 {
		files += "  (" + strings.Join(notes, ", ") + ")"
	}
	return []Row{
		R("snapshot", "%s", ShortID(snap.ID)),
		R("files", "%s", files),
		R("size", "%s", Bytes(st.TotalSize)),
		R("new", "%s, stored as %s", Bytes(st.NewChunksRawSize), Bytes(st.StoredSize)),
	}
}
