package arkdocker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	ark "github.com/alexjoedt/ark"
	"github.com/distribution/reference"
)

// SourceTypeDockerVolume is the source type of a Docker volume snapshot. The
// string is stored verbatim in the snapshot manifest.
const SourceTypeDockerVolume ark.SourceType = "docker-volume"

// dockerVolumeSource streams a Docker volume through a helper container.
type dockerVolumeSource struct {
	client *Client
	volume string
	meta   ark.SourceMeta
}

// NewVolumeSource returns a Source over the named volume, for callers that
// compose with ark.Repository directly. Engine.BackupVolume adds the container
// stop and restart around it.
func NewVolumeSource(client *Client, volume string) ark.Source {
	return newVolumeSource(client, volume, nil)
}

// newVolumeSource also records the containers that mount the volume.
func newVolumeSource(client *Client, volume string, ctrs []ContainerInfo) ark.Source {
	labels := map[string]string{}
	if client.host != "" {
		labels[ark.ReservedLabelPrefix+"docker-host"] = client.host
	}
	maps.Copy(labels, containerLabels(ctrs))
	return &dockerVolumeSource{
		client: client,
		volume: volume,
		meta: ark.SourceMeta{
			Name:   volume,
			Type:   SourceTypeDockerVolume,
			Labels: labels,
		},
	}
}

// containerLabels records per container, sorted by name, the name, image
// reference, image ID, state and compose origin as indexed keys. Empty
// values are left out.
func containerLabels(ctrs []ContainerInfo) map[string]string {
	labels := map[string]string{}
	sorted := slices.SortedFunc(slices.Values(ctrs), func(a, b ContainerInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	for i, ctr := range sorted {
		prefix := ark.ReservedLabelPrefix + "container." + strconv.Itoa(i) + "."
		for k, v := range map[string]string{
			"name":            ctr.Name,
			"image":           imageReference(ctr.Image),
			"image_id":        ctr.ImageID,
			"state":           ctr.State,
			"compose_project": ctr.ComposeProject,
			"compose_service": ctr.ComposeService,
		} {
			if v != "" {
				labels[prefix+k] = v
			}
		}
	}
	return labels
}

// imageReference appends :latest when the reference has neither tag nor
// digest. Strings that do not parse, such as raw IDs, stay as they are.
func imageReference(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil || looksLikeImageID(image) {
		return image
	}
	if _, ok := named.(reference.Tagged); ok {
		return image
	}
	if _, ok := named.(reference.Digested); ok {
		return image
	}
	return image + ":latest"
}

// Meta returns the source metadata stored in the snapshot manifest.
func (s *dockerVolumeSource) Meta() ark.SourceMeta {
	m := s.meta
	m.Labels = maps.Clone(m.Labels)
	return m
}

// Walk streams the volume's tar archive and calls fn for every entry. The
// "data/" prefix the helper container adds is stripped from paths and from
// hardlink targets. Fifos and devices are yielded with their type and device
// numbers; any other tar type, and a device number beyond 32 bits, is yielded
// as ark.FileTypeUnsupported so the core can count it.
func (s *dockerVolumeSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	rc, err := s.client.streamVolume(ctx, s.volume)
	if err != nil {
		return fmt.Errorf("arkdocker: failed to stream volume %s: %w", s.volume, err)
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("arkdocker: tar read error for volume %s: %w", s.volume, err)
		}

		path := stripRoot(hdr.Name)
		if path == "" {
			continue // the root directory itself
		}

		entry := tarEntry(hdr, path)
		var open func() (io.ReadCloser, error)
		if entry.Type == ark.FileTypeRegular {
			// tr.Next skips what the caller leaves unread, so a file
			// the parent snapshot holds unchanged costs no copy here.
			open = func() (io.ReadCloser, error) { return io.NopCloser(io.LimitReader(tr, hdr.Size)), nil }
		}
		if err := fn(entry, open); err != nil {
			return err
		}
	}
	return nil
}

// tarEntry maps a header of the volume archive to the entry Walk yields at
// path. A tar type the tree does not model, and a device whose numbers it
// cannot hold, come back as ark.FileTypeUnsupported.
func tarEntry(hdr *tar.Header, path string) ark.FileInfo {
	entry := ark.FileInfo{
		Path:    path,
		Mode:    hdr.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky),
		UID:     hdr.Uid,
		GID:     hdr.Gid,
		ModTime: hdr.ModTime,
		Type:    ark.FileTypeUnsupported,
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		entry.Type = ark.FileTypeDirectory
	case tar.TypeSymlink:
		entry.Type = ark.FileTypeSymlink
		entry.LinkDest = hdr.Linkname
	case tar.TypeLink:
		// tar names the earlier member, which is the order the core wants.
		entry.Type = ark.FileTypeHardlink
		entry.LinkDest = stripRoot(hdr.Linkname)
	case tar.TypeReg:
		entry.Type = ark.FileTypeRegular
		entry.Size = hdr.Size
	case tar.TypeFifo:
		entry.Type = ark.FileTypeFifo
	case tar.TypeChar, tar.TypeBlock:
		if hdr.Devmajor < 0 || hdr.Devmajor > math.MaxUint32 || hdr.Devminor < 0 || hdr.Devminor > math.MaxUint32 {
			break
		}
		entry.Type = ark.FileTypeCharDevice
		if hdr.Typeflag == tar.TypeBlock {
			entry.Type = ark.FileTypeBlockDevice
		}
		entry.DevMajor, entry.DevMinor = uint32(hdr.Devmajor), uint32(hdr.Devminor)
	}
	return entry
}

// stripRoot drops the "data/" root the helper container tars under. The
// root entry itself becomes "".
func stripRoot(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "data")
	name = strings.TrimPrefix(name, "/")
	return strings.TrimSuffix(name, "/")
}
