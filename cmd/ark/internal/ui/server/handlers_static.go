package server

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// staticFiles embeds the React (Vite) build directory.
// Before building, ensure ui/dist/ is built by running: cd ui && npm run build
// The build output is copied to internal/ui/server/static/ during the build process.
//
//go:embed static/*
var staticFiles embed.FS

// spaHandler implements the [http.Handler] interface to serve a single page
// application (SPA) from embedded static files. It inspects the URL path to
// locate a file within the embedded filesystem. If a file is found, it will
// be served. If not, the index.html file is served instead. This is suitable
// behavior for serving SPAs with client-side routing.
type spaHandler struct {
	staticFS  fs.FS
	indexPath string
}

// newSPAHandler creates a new SPA handler from the embedded static files.
// It returns an error if the static files or index.html cannot be found.
func newSPAHandler() (*spaHandler, error) {
	// Extract the static subdirectory from embedded files
	fsys, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("static files directory not found: %w", err)
	}

	// Verify index.html exists
	if _, err := fsys.Open("index.html"); err != nil {
		return nil, fmt.Errorf(
			"index.html not found in embedded static files - did you run 'task build-ui' or build the Docker image with the frontend?: %w",
			err,
		)
	}

	return &spaHandler{
		staticFS:  fsys,
		indexPath: "index.html",
	}, nil
}

// ServeHTTP inspects the URL path to locate a file within the static
// filesystem. If a file is found, it will be served. If not, the file
// located at the index path will be served. This is suitable behavior
// for serving an SPA (single page application).
func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Clean the URL path to prevent directory traversal
	path := filepath.Clean(r.URL.Path)

	// Remove leading slash for filesystem access
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	// Empty path means root, serve index.html
	if path == "" || path == "." {
		path = h.indexPath
	}

	// Check whether a file exists at the given path
	file, err := h.staticFS.Open(path)
	if err == nil {
		defer file.Close()
		// Check if it's a directory
		if fi, err := file.Stat(); err == nil && fi.IsDir() {
			// Directory requested, serve index.html
			http.ServeFileFS(w, r, h.staticFS, h.indexPath)
			return
		}
		// File exists, let http.FileServer handle it
		http.FileServerFS(h.staticFS).ServeHTTP(w, r)
		return
	}

	// File does not exist
	if os.IsNotExist(err) {
		// Serve index.html for client-side routing
		http.ServeFileFS(w, r, h.staticFS, h.indexPath)
		return
	}

	// If we got an error other than file not existing,
	// return a 500 internal server error
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// VerifyStaticFiles checks if the embedded static files exist.
// Returns an error if index.html is not found.
func VerifyStaticFiles() error {
	_, err := newSPAHandler()
	return err
}
