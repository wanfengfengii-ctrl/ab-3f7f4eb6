package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func makePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>13)
	}
	return b
}

// multipartRequest builds a publish request. Nil payload means no file part.
func multipartRequest(t *testing.T, fields map[string]string, payload []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if payload != nil {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="artifact"; filename="fw.bin"`)
		h.Set("Content-Type", "application/octet-stream")
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/firmware/releases", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// publish uploads payload with a correct digest and requires a 201.
func publish(t *testing.T, s *http.Handler, version, model string, payload []byte) {
	t.Helper()
	req := multipartRequest(t, map[string]string{
		"version":     version,
		"targetModel": model,
		"sha256":      sha256Hex(payload),
	}, payload)
	rec := httptest.NewRecorder()
	(*s).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %s: got %d: %s", version, rec.Code, rec.Body)
	}
}

func getArtifact(t *testing.T, s http.Handler, version string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/firmware/releases/"+version+"/artifact", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestPublishAndDownloadFull(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(100_000)

	req := multipartRequest(t, map[string]string{
		"version": "1.0.0", "targetModel": "model-a", "sha256": sha256Hex(payload),
	}, payload)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: got %d: %s", rec.Code, rec.Body)
	}
	etag := `"` + sha256Hex(payload) + `"`
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("publish ETag = %q, want %q", got, etag)
	}
	var meta ReleaseMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("publish response not JSON: %v", err)
	}
	if meta.Version != "1.0.0" || meta.TargetModel != "model-a" ||
		meta.SHA256 != sha256Hex(payload) || meta.Size != int64(len(payload)) {
		t.Errorf("unexpected metadata: %+v", meta)
	}

	rec = getArtifact(t, s, "1.0.0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: got %d", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("ETag = %q, want %q", got, etag)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Errorf("Content-Length = %q, want %d", got, len(payload))
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Error("downloaded bytes differ from uploaded bytes")
	}
}

func TestUploadSHA256Mismatch(t *testing.T) {
	s, dir := newTestServer(t)
	payload := makePayload(4096)

	req := multipartRequest(t, map[string]string{
		"version": "1.0.0", "targetModel": "m", "sha256": strings.Repeat("0", 64),
	}, payload)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", rec.Code, rec.Body)
	}
	// The store must not be polluted.
	entries, err := os.ReadDir(filepath.Join(dir, "releases"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("release store polluted: entries=%v err=%v", entries, err)
	}
	tmpEntries, err := os.ReadDir(filepath.Join(dir, ".tmp"))
	if err != nil || len(tmpEntries) != 0 {
		t.Fatalf("temp upload left behind: entries=%v err=%v", tmpEntries, err)
	}
	// And nothing is downloadable.
	rec = getArtifact(t, s, "1.0.0", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("artifact after mismatch: got %d, want 404", rec.Code)
	}
}

func TestUploadDuplicateVersion(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(2048)
	var h http.Handler = s
	publish(t, &h, "1.0.0", "m", payload)

	// Same version, different bytes must be rejected...
	other := makePayload(512)
	req := multipartRequest(t, map[string]string{
		"version": "1.0.0", "targetModel": "m", "sha256": sha256Hex(other),
	}, other)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: got %d, want 409: %s", rec.Code, rec.Body)
	}

	// ...and the original bytes must be untouched.
	rec = getArtifact(t, s, "1.0.0", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("original release corrupted after duplicate attempt: code=%d", rec.Code)
	}
}

func TestUploadValidation(t *testing.T) {
	payload := makePayload(128)
	good := sha256Hex(payload)

	cases := []struct {
		name       string
		fields     map[string]string
		payload    []byte
		wantStatus int
	}{
		{"missing sha256", map[string]string{"version": "1.0.0", "targetModel": "m"}, payload, 400},
		{"missing version", map[string]string{"targetModel": "m", "sha256": good}, payload, 400},
		{"missing targetModel", map[string]string{"version": "1.0.0", "sha256": good}, payload, 400},
		{"missing artifact", map[string]string{"version": "1.0.0", "targetModel": "m", "sha256": good}, nil, 400},
		{"empty version", map[string]string{"version": "", "targetModel": "m", "sha256": good}, payload, 400},
		{"dotdot version", map[string]string{"version": "..", "targetModel": "m", "sha256": good}, payload, 400},
		{"space in version", map[string]string{"version": "1.0 beta", "targetModel": "m", "sha256": good}, payload, 400},
		{"slash in version", map[string]string{"version": "1/0", "targetModel": "m", "sha256": good}, payload, 400},
		{"short sha256", map[string]string{"version": "1.0.0", "targetModel": "m", "sha256": "abcd"}, payload, 400},
		{"non-hex sha256", map[string]string{"version": "1.0.0", "targetModel": "m", "sha256": strings.Repeat("z", 64)}, payload, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := newTestServer(t)
			req := multipartRequest(t, tc.fields, tc.payload)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			entries, _ := os.ReadDir(filepath.Join(dir, "releases"))
			if len(entries) != 0 {
				t.Fatal("release store polluted by rejected upload")
			}
		})
	}

	t.Run("not multipart", func(t *testing.T) {
		s, _ := newTestServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/firmware/releases", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("got %d, want 415", rec.Code)
		}
	})
}

func TestRangeRequests(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(1000)
	var h http.Handler = s
	publish(t, &h, "2.0.0", "m", payload)
	etag := `"` + sha256Hex(payload) + `"`

	cases := []struct {
		name        string
		rangeHdr    string
		wantStatus  int
		wantCR      string
		wantBody    []byte // nil: don't check
		checkLength bool
	}{
		{"closed", "bytes=0-99", 206, "bytes 0-99/1000", payload[0:100], true},
		{"closed end clamped", "bytes=900-5000", 206, "bytes 900-999/1000", payload[900:], true},
		{"single byte", "bytes=0-0", 206, "bytes 0-0/1000", payload[0:1], true},
		{"last byte", "bytes=999-999", 206, "bytes 999-999/1000", payload[999:], true},
		{"open ended", "bytes=900-", 206, "bytes 900-999/1000", payload[900:], true},
		{"suffix", "bytes=-50", 206, "bytes 950-999/1000", payload[950:], true},
		{"suffix oversize", "bytes=-5000", 206, "bytes 0-999/1000", payload, true},
		{"past eof open", "bytes=1000-", 416, "bytes */1000", nil, false},
		{"past eof closed", "bytes=1000-2000", 416, "bytes */1000", nil, false},
		{"start after end", "bytes=200-100", 416, "bytes */1000", nil, false},
		{"zero suffix", "bytes=-0", 416, "bytes */1000", nil, false},
		{"garbage", "bytes=abc", 416, "bytes */1000", nil, false},
		{"empty", "bytes=", 416, "bytes */1000", nil, false},
		{"no dash", "bytes=5", 416, "bytes */1000", nil, false},
		{"negative", "bytes=-5-", 416, "bytes */1000", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getArtifact(t, s, "2.0.0", map[string]string{"Range": tc.rangeHdr})
			if rec.Code != tc.wantStatus {
				t.Fatalf("got %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.wantCR {
				t.Errorf("Content-Range = %q, want %q", got, tc.wantCR)
			}
			if tc.wantStatus == 206 {
				if got := rec.Header().Get("ETag"); got != etag {
					t.Errorf("ETag = %q, want %q", got, etag)
				}
				if !bytes.Equal(rec.Body.Bytes(), tc.wantBody) {
					t.Errorf("body mismatch: got %d bytes", rec.Body.Len())
				}
				if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(tc.wantBody)) {
					t.Errorf("Content-Length = %q, want %d", got, len(tc.wantBody))
				}
			}
		})
	}
}

func TestIfRange(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(1000)
	var h http.Handler = s
	publish(t, &h, "3.0.0", "m", payload)
	etag := `"` + sha256Hex(payload) + `"`

	t.Run("matching etag", func(t *testing.T) {
		rec := getArtifact(t, s, "3.0.0", map[string]string{
			"Range": "bytes=0-9", "If-Range": etag,
		})
		if rec.Code != http.StatusPartialContent || rec.Body.Len() != 10 {
			t.Fatalf("got %d with %d bytes, want 206 with 10 bytes", rec.Code, rec.Body.Len())
		}
	})

	t.Run("stale etag yields full body", func(t *testing.T) {
		rec := getArtifact(t, s, "3.0.0", map[string]string{
			"Range": "bytes=0-9", "If-Range": `"` + strings.Repeat("0", 64) + `"`,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Error("If-Range mismatch did not return the full representation")
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
			t.Errorf("Content-Length = %q, want %d", got, len(payload))
		}
	})
}

func TestHeadRequest(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(777)
	var h http.Handler = s
	publish(t, &h, "4.0.0", "m", payload)

	req := httptest.NewRequest(http.MethodHead, "/api/firmware/releases/4.0.0/artifact", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "777" {
		t.Errorf("Content-Length = %q, want 777", got)
	}
	if got := rec.Header().Get("ETag"); got != `"`+sha256Hex(payload)+`"` {
		t.Errorf("ETag = %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes", rec.Body.Len())
	}
}

func TestRestartKeepsReleases(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := makePayload(5000)
	var h http.Handler = s1
	publish(t, &h, "5.0.0", "m", payload)

	// Simulate a restart: a brand new Server over the same data directory.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := getArtifact(t, s2, "5.0.0", map[string]string{"Range": "bytes=100-199"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("got %d, want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[100:200]) {
		t.Error("segment after restart does not match original bytes")
	}
	if got := rec.Header().Get("ETag"); got != `"`+sha256Hex(payload)+`"` {
		t.Errorf("ETag after restart = %q", got)
	}
}

func TestUnknownAndInvalidVersions(t *testing.T) {
	s, _ := newTestServer(t)

	rec := getArtifact(t, s, "9.9.9", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown version: got %d, want 404", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/firmware/releases/v!x/artifact", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid version: got %d, want 400", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t)
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/firmware/releases/1.0.0", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want 405", method, rec.Code)
		}
	}
}

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestListAndGetRelease(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(64)
	var h http.Handler = s
	publish(t, &h, "6.0.0", "model-z", payload)

	req := httptest.NewRequest(http.MethodGet, "/api/firmware/releases", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "6.0.0") {
		t.Fatalf("list: got %d: %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/firmware/releases/6.0.0", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get release: got %d", rec.Code)
	}
	var meta ReleaseMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.TargetModel != "model-z" || meta.Size != 64 || meta.SHA256 != sha256Hex(payload) {
		t.Errorf("unexpected metadata: %+v", meta)
	}
}

func TestSegmentedReassembly(t *testing.T) {
	s, _ := newTestServer(t)
	payload := makePayload(2*1024*1024 + 7)
	var h http.Handler = s
	publish(t, &h, "7.0.0", "m", payload)

	// Download the artifact in uneven contiguous segments and reassemble.
	size := int64(len(payload))
	cuts := []int64{0, 1, 4097, 65536, 1 << 20, size - 13, size}
	var reassembled []byte
	for i := 0; i+1 < len(cuts); i++ {
		start, end := cuts[i], cuts[i+1]-1
		rec := getArtifact(t, s, "7.0.0", map[string]string{
			"Range": fmt.Sprintf("bytes=%d-%d", start, end),
		})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("segment %d-%d: got %d", start, end, rec.Code)
		}
		wantCR := fmt.Sprintf("bytes %d-%d/%d", start, end, size)
		if got := rec.Header().Get("Content-Range"); got != wantCR {
			t.Fatalf("segment %d-%d: Content-Range = %q, want %q", start, end, got, wantCR)
		}
		reassembled = append(reassembled, rec.Body.Bytes()...)
	}
	if !bytes.Equal(reassembled, payload) {
		t.Fatal("reassembled artifact differs from published bytes")
	}
	if sha256Hex(reassembled) != sha256Hex(payload) {
		t.Fatal("reassembled digest mismatch")
	}
}
