package arkdocker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"

	ark "github.com/alexjoedt/ark"
)

// dockerVolumeSink assembles the entries into a tar stream and pipes it into
// a helper container that extracts it into the volume.
//
// Open rejects incompatible source types, then opens an [io.Pipe] and starts
// the restore goroutine. Receive writes one entry per call. Close flushes the
// tar stream, closes the pipe and waits for the goroutine.
type dockerVolumeSink struct {
	client *Client
	volume string

	// set in Open
	tw   *tar.Writer
	pw   *io.PipeWriter
	done chan error
}

// NewVolumeSink returns a Sink that restores into the named volume, for
// callers that compose with ark.Repository directly. Engine.RestoreVolume
// adds the container stop and restart around it.
func NewVolumeSink(client *Client, volume string) ark.Sink {
	return &dockerVolumeSink{client: client, volume: volume}
}

// Open validates the snapshot source type and starts the Docker restore goroutine.
func (s *dockerVolumeSink) Open(ctx context.Context, meta ark.SourceMeta) error {
	if meta.Type != SourceTypeDockerVolume {
		return fmt.Errorf("%w: expected %q, got %q",
			ark.ErrIncompatibleSourceType, SourceTypeDockerVolume, meta.Type)
	}

	pr, pw := io.Pipe()
	tw := tar.NewWriter(pw)
	done := make(chan error, 1)

	go func() {
		err := s.client.restoreVolume(ctx, s.volume, pr)
		// Drain the reader in case the pipeline stopped early.
		_, _ = io.Copy(io.Discard, pr)
		_ = pr.CloseWithError(err)
		done <- err
	}()

	s.tw = tw
	s.pw = pw
	s.done = done
	return nil
}

// Receive writes one filesystem entry into the tar stream. Unsupported
// entries never reach a sink, the core drops them during backup.
func (s *dockerVolumeSink) Receive(_ context.Context, entry ark.FileInfo, r io.Reader) error {
	if entry.Type == ark.FileTypeUnsupported {
		return nil
	}
	hdr := fileInfoToTarHeader(entry)
	if err := s.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("arkdocker: sink write header for %s: %w", entry.Path, err)
	}
	if entry.Type == ark.FileTypeRegular && r != nil {
		if _, err := io.Copy(s.tw, r); err != nil {
			return fmt.Errorf("arkdocker: sink write data for %s: %w", entry.Path, err)
		}
	}
	return nil
}

// Close flushes the tar footer, closes the pipe, and waits for Docker to finish.
func (s *dockerVolumeSink) Close() error {
	if err := s.tw.Close(); err != nil {
		_ = s.pw.CloseWithError(err)
		return fmt.Errorf("arkdocker: sink tar close: %w", err)
	}
	if err := s.pw.Close(); err != nil {
		return fmt.Errorf("arkdocker: sink pipe close: %w", err)
	}
	return <-s.done
}

// fileInfoToTarHeader converts an ark.FileInfo to a [tar.Header]. Paths are
// relative to the volume root, the restore container extracts into it.
func fileInfoToTarHeader(fi ark.FileInfo) *tar.Header {
	hdr := &tar.Header{
		Name:    fi.Path,
		Mode:    int64(fi.Mode.Perm()),
		Uid:     fi.UID,
		Gid:     fi.GID,
		ModTime: fi.ModTime,
	}
	switch fi.Type {
	case ark.FileTypeDirectory:
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
	case ark.FileTypeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = fi.LinkDest
	case ark.FileTypeHardlink:
		// LinkDest names an entry already restored; the core hands hardlinks
		// to the sink after their target.
		hdr.Typeflag = tar.TypeLink
		hdr.Linkname = fi.LinkDest
	case ark.FileTypeRegular, ark.FileTypeUnsupported:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = fi.Size
	}
	return hdr
}
