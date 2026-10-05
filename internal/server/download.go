package server

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// handleGetArtifact serves the firmware bytes of a published release.
//
// http.ServeContent provides the RFC 9110 range machinery (single closed,
// open-ended and suffix ranges, multi-range, If-Range, HEAD, 200/206/416).
// On top of it this handler adds two strictness guarantees the API
// contract requires:
//
//   - a malformed single range is rejected with 416 instead of being
//     silently ignored, and
//   - every 416 response carries a Content-Range header.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if !versionRe.MatchString(version) {
		writeError(w, http.StatusBadRequest, "invalid version identifier")
		return
	}
	meta, err := s.loadMeta(version)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "release not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot read release metadata")
		}
		return
	}

	artifactPath := filepath.Join(s.releaseDir(version), artifactName)
	f, err := os.Open(artifactPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "artifact not found")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot open artifact")
		}
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stat artifact")
		return
	}

	// The ETag is the verified SHA-256 of the artifact: identical bytes
	// always yield an identical tag, and any change yields a different one.
	etag := strconv.Quote(meta.SHA256)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/octet-stream")

	// Reject malformed single ranges ourselves, but only when If-Range
	// permits a partial response; otherwise the Range header is ignored
	// and the full representation is sent, per RFC 9110 section 14.2.
	if rangeHeaderInvalid(r.Header.Get("Range")) &&
		ifRangeAllows(r.Header.Get("If-Range"), etag, fi.ModTime()) {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(fi.Size(), 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid range")
		return
	}

	http.ServeContent(&ensureContentRange{ResponseWriter: w, size: fi.Size()},
		r, artifactName, fi.ModTime(), f)
}

// ensureContentRange guarantees every 416 response carries a Content-Range
// header, including the cases http.ServeContent reports without one.
type ensureContentRange struct {
	http.ResponseWriter
	size int64
}

func (w *ensureContentRange) WriteHeader(code int) {
	if code == http.StatusRequestedRangeNotSatisfiable && w.Header().Get("Content-Range") == "" {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(w.size, 10))
	}
	w.ResponseWriter.WriteHeader(code)
}

var (
	singleRangeRe = regexp.MustCompile(`^bytes=(?:\d+-\d*|-\d+)$`)
	zeroSuffixRe  = regexp.MustCompile(`^bytes=-0+$`)
)

// rangeHeaderInvalid reports whether a Range header is a malformed or
// unsatisfiable-by-construction single byte-range. Unknown units and
// multi-range requests are left to http.ServeContent, which ignores the
// former and answers the latter with multipart/byteranges.
func rangeHeaderInvalid(h string) bool {
	if h == "" {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(h), "bytes=") {
		return false // unknown range unit: ignored per RFC 9110
	}
	if strings.Contains(h, ",") {
		return false // multi-range: handled as multipart/byteranges
	}
	return !singleRangeRe.MatchString(h) || zeroSuffixRe.MatchString(h)
}

// ifRangeAllows reports whether an If-Range header permits a partial
// response, mirroring the strong-comparison and HTTP-date semantics of
// net/http's own conditional handling.
func ifRangeAllows(ifRange, etag string, modTime time.Time) bool {
	if ifRange == "" {
		return true
	}
	ir := strings.TrimSpace(ifRange)
	tag := strings.TrimPrefix(ir, "W/")
	if strings.HasPrefix(tag, `"`) {
		return tag == etag && etag != ""
	}
	if t, err := http.ParseTime(ir); err == nil {
		return t.Unix() == modTime.Unix()
	}
	return false
}
