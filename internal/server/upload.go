package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

const maxFieldSize = 8 << 10 // 8 KiB per text field

var errVersionExists = errors.New("version already exists")

// handleCreateRelease publishes a new firmware release.
//
// The artifact is streamed to a temp file while its SHA-256 is computed; the
// release directory is only claimed (atomically) after the digest matches the
// publisher-supplied sha256 field, so a bad upload never touches the store
// and an already published version is never overwritten.
func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		writeError(w, http.StatusUnsupportedMediaType,
			"Content-Type must be multipart/form-data")
		return
	}

	u, err := s.streamUpload(multipart.NewReader(r.Body, params["boundary"]))
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds the %d byte limit", s.maxUpload))
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	// From here on a temp file exists and must be removed on any failure.
	abort := func(status int, msg string) {
		_ = os.Remove(u.tmpPath)
		writeError(w, status, msg)
	}

	if u.version == "" || u.targetModel == "" || u.sha256 == "" {
		abort(http.StatusBadRequest, "missing required field(s): version, targetModel, sha256")
		return
	}
	if !versionRe.MatchString(u.version) {
		abort(http.StatusBadRequest,
			"invalid version: must match "+versionRe.String())
		return
	}
	if !sha256Re.MatchString(u.sha256) {
		abort(http.StatusBadRequest, "invalid sha256: must be 64 hexadecimal characters")
		return
	}
	sha := strings.ToLower(u.sha256)

	if _, err := os.Stat(s.releaseDir(u.version)); err == nil {
		abort(http.StatusConflict, "version "+u.version+" is already published")
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		abort(http.StatusInternalServerError, "cannot inspect release store")
		return
	}

	if u.digest != sha {
		abort(http.StatusUnprocessableEntity, fmt.Sprintf(
			"sha256 mismatch: computed %s but declared %s; artifact discarded, nothing stored",
			u.digest, sha))
		return
	}

	meta := ReleaseMeta{
		Version:     u.version,
		TargetModel: u.targetModel,
		SHA256:      sha,
		Size:        u.size,
		UploadedAt:  time.Now().UTC(),
	}
	if err := s.commit(u.version, u.tmpPath, meta); err != nil {
		_ = os.Remove(u.tmpPath)
		if errors.Is(err, errVersionExists) {
			writeError(w, http.StatusConflict, "version "+u.version+" is already published")
			return
		}
		s.log.Error("commit failed", "version", u.version, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to store release")
		return
	}

	w.Header().Set("ETag", strconv.Quote(sha))
	w.Header().Set("Location", "/api/firmware/releases/"+u.version)
	writeJSON(w, http.StatusCreated, meta)
}

// commit atomically publishes a release: it claims the version directory,
// moves the verified artifact in place and writes the metadata. The version
// directory is created with O_EXCL semantics, which is what makes a
// published version impossible to overwrite even under concurrent uploads.
func (s *Server) commit(version, tmpPath string, meta ReleaseMeta) error {
	dir := s.releaseDir(version)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errVersionExists
		}
		return err
	}
	rollback := func() { _ = os.RemoveAll(dir) }

	if err := os.Rename(tmpPath, filepath.Join(dir, artifactName)); err != nil {
		rollback()
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		rollback()
		return err
	}
	metaTmp := filepath.Join(dir, ".meta.tmp")
	if err := os.WriteFile(metaTmp, data, 0o644); err != nil {
		rollback()
		return err
	}
	if err := os.Rename(metaTmp, filepath.Join(dir, metaName)); err != nil {
		rollback()
		return err
	}
	// Best-effort durability for the directory entries themselves.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// upload holds the result of consuming a multipart publish request.
type upload struct {
	version     string
	targetModel string
	sha256      string
	tmpPath     string // verified-or-not artifact bytes, outside the store
	digest      string // computed SHA-256 of the received artifact
	size        int64
}

// streamUpload consumes the multipart body, saving the artifact part to a
// temp file while hashing it, and collecting the small text fields. Field
// order is not assumed: the digest is compared only after the whole body
// has been read.
func (s *Server) streamUpload(mr *multipart.Reader) (upload, error) {
	var u upload
	cleanup := func() {
		if u.tmpPath != "" {
			_ = os.Remove(u.tmpPath)
		}
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return u, fmt.Errorf("malformed multipart body: %w", err)
		}

		switch part.FormName() {
		case "version", "targetModel", "sha256":
			data, err := io.ReadAll(io.LimitReader(part, maxFieldSize+1))
			if err != nil {
				cleanup()
				return u, err
			}
			if len(data) > maxFieldSize {
				cleanup()
				return u, fmt.Errorf("field %q exceeds %d bytes", part.FormName(), maxFieldSize)
			}
			switch part.FormName() {
			case "version":
				u.version = strings.TrimSpace(string(data))
			case "targetModel":
				u.targetModel = strings.TrimSpace(string(data))
			case "sha256":
				u.sha256 = strings.TrimSpace(string(data))
			}

		case "artifact":
			if u.tmpPath != "" {
				cleanup()
				return u, fmt.Errorf("multiple artifact parts")
			}
			tmp, err := os.CreateTemp(s.tmpDir(), "upload-*")
			if err != nil {
				return u, err
			}
			h := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(tmp, h), part)
			syncErr := tmp.Sync()
			closeErr := tmp.Close()
			if copyErr != nil {
				_ = os.Remove(tmp.Name())
				return u, copyErr
			}
			if syncErr != nil || closeErr != nil {
				_ = os.Remove(tmp.Name())
				return u, fmt.Errorf("cannot persist upload")
			}
			u.tmpPath = tmp.Name()
			u.digest = hex.EncodeToString(h.Sum(nil))
			u.size = n

		default:
			// Unknown parts are ignored but still drained.
			if _, err := io.Copy(io.Discard, part); err != nil {
				cleanup()
				return u, err
			}
		}
		_ = part.Close()
	}

	if u.tmpPath == "" {
		return u, fmt.Errorf("missing artifact file part")
	}
	return u, nil
}
