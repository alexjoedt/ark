package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	maxUploadSize = 10 * 1024 * 1024 * 1024 // 10GB default
)

// handleExportSnapshot streams a snapshot as a portable tar to the client.
// The archive is reconstructed on-the-fly from CDC chunks — no file on disk.
func (s *Server) handleExportSnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := s.transferContext(w, r)

	snap, err := s.repo.Snapshot(ctx, id)
	if err != nil {
		s.respondError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	id = snap.ID

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".tar"))

	if err := s.repo.Export(ctx, id, w); err != nil {
		// Headers are already sent; log the error but cannot change status.
		s.logger.Error("failed to export snapshot", "id", id, "error", err)
	}
}

// handleImportSnapshot accepts a tar upload, gzipped or not, and ingests it
// into the CDC store.
func (s *Server) handleImportSnapshot(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	ctx := s.transferContext(w, r)

	// Support both a raw body and a multipart form. Import sniffs gzip itself,
	// so the archive may be compressed either way.
	switch r.Header.Get("Content-Type") {
	case "application/x-tar", "application/tar", "application/gzip", "application/x-gzip":
		snap, err := s.repo.Import(ctx, r.Body)
		if err != nil {
			s.logger.Error("failed to import snapshot", "error", err)
			s.respondError(w, http.StatusBadRequest, fmt.Sprintf("import failed: %v", err))
			return
		}
		s.respondJSON(w, http.StatusCreated, snap)
		return
	}

	// Multipart form upload
	//nolint:gosec // uploads are files of any size, the form spills to disk past 32 MiB
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.respondError(w, http.StatusBadRequest, "file too large or invalid form")
		return
	}

	file, _, err := r.FormFile("archive")
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "no file uploaded")
		return
	}
	defer file.Close()

	snap, err := s.repo.Import(ctx, file)
	if err != nil {
		s.logger.Error("failed to import snapshot", "error", err)
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("import failed: %v", err))
		return
	}

	s.respondJSON(w, http.StatusCreated, snap)
}

// transferContext lifts the server's read and write deadlines and the
// router's request timeout for one archive transfer, which takes as long as
// the archive is large. A client that goes away still ends it: the next read
// or write fails.
func (s *Server) transferContext(w http.ResponseWriter, r *http.Request) context.Context {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.logger.Warn("cannot lift read deadline", "error", err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Warn("cannot lift write deadline", "error", err)
	}
	return context.WithoutCancel(r.Context())
}
