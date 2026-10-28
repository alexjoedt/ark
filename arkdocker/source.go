package arkdocker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"maps"
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
		labels["docker_host"] = client.host
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
		prefix := "container." + strconv.Itoa(i) + "."
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
func (s *dockerVolumeSource) Meta() ark.SourceMeta { return s.meta }

// Walk streams the volume's tar archive and calls fn for every entry. The
// "data/" prefix the helper container adds is stripped from paths and from
// hardlink targets. Devices, fifos and sockets are yielded as
// ark.FileTypeUnsupported so the core can count them.
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

		entry := ark.FileInfo{
			Path:    path,
			Mode:    hdr.FileInfo().Mode().Perm(),
			UID:     hdr.Uid,
			GID:     hdr.Gid,
			ModTime: hdr.ModTime,
		}

		var r io.Reader
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
			r = io.LimitReader(tr, hdr.Size)
		default:
			entry.Type = ark.FileTypeUnsupported
		}
		if err := fn(entry, r); err != nil {
			return err
		}
	}
	return nil
}

// stripRoot drops the "data/" root the helper container tars under. The
// root entry itself becomes "".
func stripRoot(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "data")
	name = strings.TrimPrefix(name, "/")
	return strings.TrimSuffix(name, "/")
}
