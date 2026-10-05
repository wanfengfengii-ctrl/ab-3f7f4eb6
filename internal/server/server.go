// Package server implements the firmware release registry HTTP API.
//
// Releases are stored on a persistent volume rooted at a data directory:
//
//	<dataDir>/releases/<version>/artifact.bin   verified firmware bytes
//	<dataDir>/releases/<version>/meta.json      release metadata
//	<dataDir>/.tmp/                             in-flight uploads (never published)
//
// A release becomes visible only after its artifact has been fully received,
// its SHA-256 verified against the publisher-supplied digest, and the version
// directory atomically claimed, so readers never observe partial or
// unverified firmware and a published version can never be overwritten.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

const (
	// DefaultMaxUpload bounds a single artifact upload in bytes.
	DefaultMaxUpload int64 = 1 << 30 // 1 GiB

	releasesDirName = "releases"
	tmpDirName      = ".tmp"
	artifactName    = "artifact.bin"
	metaName        = "meta.json"
)

// versionRe keeps version identifiers safe to use as a single path segment.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ReleaseMeta describes a published firmware release.
type ReleaseMeta struct {
	Version     string    `json:"version"`
	TargetModel string    `json:"targetModel"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	UploadedAt  time.Time `json:"uploadedAt"`
}

// Server is the firmware release registry HTTP handler.
type Server struct {
	dataDir   string
	maxUpload int64
	log       *slog.Logger
	mux       *http.ServeMux
}

// Option customizes a Server.
type Option func(*Server)

// WithMaxUpload sets the maximum accepted artifact size in bytes.
func WithMaxUpload(n int64) Option {
	return func(s *Server) { s.maxUpload = n }
}

// WithLogger sets the logger used for operational messages.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// New creates a Server rooted at dataDir, creating the directory layout and
// removing any incomplete state left behind by an interrupted run. Releases
// published in previous runs remain downloadable.
func New(dataDir string, opts ...Option) (*Server, error) {
	s := &Server{
		dataDir:   dataDir,
		maxUpload: DefaultMaxUpload,
		log:       slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := os.MkdirAll(s.releasesDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create releases dir: %w", err)
	}
	if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	if err := s.scrubStore(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/firmware/releases", s.handleCreateRelease)
	mux.HandleFunc("GET /api/firmware/releases", s.handleListReleases)
	mux.HandleFunc("GET /api/firmware/releases/{version}", s.handleGetRelease)
	mux.HandleFunc("GET /api/firmware/releases/{version}/artifact", s.handleGetArtifact)
	s.mux = mux
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) releasesDir() string { return filepath.Join(s.dataDir, releasesDirName) }
func (s *Server) tmpDir() string      { return filepath.Join(s.dataDir, tmpDirName) }

func (s *Server) releaseDir(version string) string {
	return filepath.Join(s.releasesDir(), version)
}

// scrubStore removes stale temp uploads and half-published releases so the
// store only ever contains complete, verified releases.
func (s *Server) scrubStore() error {
	tmpEntries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		return fmt.Errorf("scan tmp dir: %w", err)
	}
	for _, e := range tmpEntries {
		p := filepath.Join(s.tmpDir(), e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove stale temp upload %s: %w", p, err)
		}
		s.log.Warn("removed stale temp upload", "path", p)
	}

	entries, err := os.ReadDir(s.releasesDir())
	if err != nil {
		return fmt.Errorf("scan releases dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := s.releaseDir(e.Name())
		if _, err := s.loadMeta(e.Name()); err != nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				return fmt.Errorf("remove incomplete release %s: %w", dir, rmErr)
			}
			s.log.Warn("removed incomplete release", "version", e.Name())
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, artifactName)); err != nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				return fmt.Errorf("remove incomplete release %s: %w", dir, rmErr)
			}
			s.log.Warn("removed incomplete release", "version", e.Name())
		}
	}
	return nil
}

func (s *Server) loadMeta(version string) (ReleaseMeta, error) {
	var m ReleaseMeta
	data, err := os.ReadFile(filepath.Join(s.releaseDir(version), metaName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("corrupt metadata for %q: %w", version, err)
	}
	return m, nil
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeError sends a JSON error body with the given status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Confirm the store is still usable so a broken volume fails the check.
	if _, err := os.Stat(s.releasesDir()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "data directory unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.releasesDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list releases")
		return
	}
	metas := make([]ReleaseMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m, err := s.loadMeta(e.Name()); err == nil {
			metas = append(metas, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": metas})
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if !versionRe.MatchString(version) {
		writeError(w, http.StatusBadRequest, "invalid version identifier")
		return
	}
	m, err := s.loadMeta(version)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "release not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot read release metadata")
		}
		return
	}
	w.Header().Set("ETag", strconv.Quote(m.SHA256))
	writeJSON(w, http.StatusOK, m)
}
