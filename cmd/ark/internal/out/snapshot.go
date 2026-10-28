package out

import (
	"fmt"
	"time"

	"github.com/alexjoedt/ark"
)

// Snapshot is the JSON shape of a snapshot, frozen for 1.x. The
// library struct is mapped here so its field names can move without
// breaking scripts.
type Snapshot struct {
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	SourceName string            `json:"source_name"`
	SourceType string            `json:"source_type"`
	Host       string            `json:"host"`
	Tags       []string          `json:"tags"`
	Comment    string            `json:"comment"`
	ParentID   string            `json:"parent_id"`
	Stats      ark.SnapshotStats `json:"stats"`
}

// NewSnapshot maps s. Tags is never null.
func NewSnapshot(s *ark.Snapshot) Snapshot {
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	return Snapshot{
		ID:         s.ID,
		CreatedAt:  s.CreatedAt,
		SourceName: s.SourceName,
		SourceType: string(s.SourceType),
		Host:       s.Host,
		Tags:       tags,
		Comment:    s.Comment,
		ParentID:   s.ParentID,
		Stats:      s.Stats,
	}
}

// Snapshots maps a list. The result is never null.
func Snapshots(list []*ark.Snapshot) []Snapshot {
	res := make([]Snapshot, 0, len(list))
	for _, s := range list {
		res = append(res, NewSnapshot(s))
	}
	return res
}

// BackupSummary is the line after a backup: files, source size, the data new
// after deduplication, what it took on the backend compressed, and the time.
func BackupSummary(label string, st ark.SnapshotStats, d time.Duration) string {
	files := Count(st.FileCount) + " files"
	if st.Skipped > 0 {
		files += fmt.Sprintf(" (%s skipped)", Count(st.Skipped))
	}
	return fmt.Sprintf("%s: %s, %s, %s new, %s stored, %s",
		label, files, Bytes(st.TotalSize), Bytes(st.NewChunksRawSize),
		Bytes(st.StoredSize), Duration(d))
}
