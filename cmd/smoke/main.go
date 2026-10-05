// Command smoke exercises a running firmware release service end to end:
// it publishes a release, downloads it in segments, reassembles the bytes
// and verifies digests, ranges, ETags and error handling. It exits 0 only
// when every check passes.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

var failures int

func fail(format string, args ...any) {
	failures++
	fmt.Printf("FAIL: "+format+"\n", args...)
}

func pass(format string, args ...any) {
	fmt.Printf("ok:   "+format+"\n", args...)
}

func check(cond bool, format string, args ...any) {
	if cond {
		pass(format, args...)
	} else {
		fail(format, args...)
	}
}

func main() {
	base := strings.TrimRight(envOr("APP_URL", "http://localhost:8080"), "/")
	fmt.Println("smoke: target", base)
	waitHealthy(base)

	version := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	payload := makePayload(2*1024*1024 + 123) // odd size on purpose
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	etag := `"` + sha + `"`
	size := int64(len(payload))

	// ---- publish -----------------------------------------------------
	status, headers, body := uploadRelease(base, version, "wtg-controller-x9", sha, payload)
	check(status == http.StatusCreated, "publish release -> 201 (got %d: %s)", status, body)
	check(headers.Get("ETag") == etag, "publish ETag == %s (got %q)", etag, headers.Get("ETag"))

	// ---- a published version must not be overwritable ----------------
	other := makePayload(1024)
	otherSum := sha256.Sum256(other)
	status, _, body = uploadRelease(base, version, "wtg-controller-x9", hex.EncodeToString(otherSum[:]), other)
	check(status == http.StatusConflict, "re-publish same version -> 409 (got %d: %s)", status, body)

	// ---- digest mismatch must be rejected and must not persist -------
	badVersion := version + "-bad"
	status, _, body = uploadRelease(base, badVersion, "wtg-controller-x9", strings.Repeat("0", 64), makePayload(4096))
	check(status == http.StatusUnprocessableEntity, "sha256 mismatch -> 422 (got %d: %s)", status, body)
	status, _, _ = get(base, "/api/firmware/releases/"+badVersion+"/artifact", nil)
	check(status == http.StatusNotFound, "mismatched release left nothing downloadable (got %d)", status)
	status, _, _ = get(base, "/api/firmware/releases/"+badVersion, nil)
	check(status == http.StatusNotFound, "mismatched release left no metadata (got %d)", status)

	// ---- invalid input -------------------------------------------------
	status, _, body = uploadRelease(base, "bad version!", "m", sha, payload)
	check(status == http.StatusBadRequest, "invalid version -> 400 (got %d: %s)", status, body)
	status, _, body = uploadRelease(base, version+"-x", "m", "not-hex", payload)
	check(status == http.StatusBadRequest, "malformed sha256 -> 400 (got %d: %s)", status, body)
	status, _, _ = get(base, "/api/firmware/releases/does-not-exist/artifact", nil)
	check(status == http.StatusNotFound, "unknown version -> 404 (got %d)", status)

	// ---- metadata ------------------------------------------------------
	status, _, body = get(base, "/api/firmware/releases/"+version, nil)
	check(status == http.StatusOK, "get release metadata -> 200 (got %d)", status)
	var meta struct {
		Version     string `json:"version"`
		TargetModel string `json:"targetModel"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		fail("metadata is not valid JSON: %s", body)
	} else {
		check(meta.Version == version && meta.SHA256 == sha && meta.Size == size,
			"metadata matches published release")
	}
	status, _, body = get(base, "/api/firmware/releases", nil)
	check(status == http.StatusOK && strings.Contains(string(body), version),
		"release list contains the new version")

	// ---- HEAD ------------------------------------------------------------
	status, headers, body = doRequest(http.MethodHead, base+"/api/firmware/releases/"+version+"/artifact", nil)
	check(status == http.StatusOK, "HEAD artifact -> 200 (got %d)", status)
	check(headers.Get("ETag") == etag, "HEAD ETag == %s (got %q)", etag, headers.Get("ETag"))
	check(headers.Get("Content-Length") == strconv.FormatInt(size, 10),
		"HEAD Content-Length == %d (got %q)", size, headers.Get("Content-Length"))
	check(headers.Get("Accept-Ranges") == "bytes", "HEAD Accept-Ranges == bytes (got %q)",
		headers.Get("Accept-Ranges"))
	check(len(body) == 0, "HEAD returns no body")

	// ---- full download ---------------------------------------------------
	status, headers, full := get(base, "/api/firmware/releases/"+version+"/artifact", nil)
	check(status == http.StatusOK, "GET artifact -> 200 (got %d)", status)
	check(headers.Get("ETag") == etag, "GET ETag == %s (got %q)", etag, headers.Get("ETag"))
	check(headers.Get("Content-Length") == strconv.FormatInt(size, 10),
		"GET Content-Length == %d (got %q)", size, headers.Get("Content-Length"))
	check(bytes.Equal(full, payload) && sha256Hex(full) == sha, "GET body is byte-identical")

	// ---- segmented download + reassembly (the resumable path) ------------
	segments := splitPlan(size)
	reassembled := make([]byte, 0, size)
	segOK := true
	for i, seg := range segments {
		st, h, b := get(base, "/api/firmware/releases/"+version+"/artifact",
			map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", seg[0], seg[1])})
		wantCR := fmt.Sprintf("bytes %d-%d/%d", seg[0], seg[1], size)
		switch {
		case st != http.StatusPartialContent:
			fail("segment %d -> 206 (got %d)", i, st)
		case h.Get("Content-Range") != wantCR:
			fail("segment %d Content-Range == %q (got %q)", i, wantCR, h.Get("Content-Range"))
		case h.Get("ETag") != etag:
			fail("segment %d ETag == %s (got %q)", i, etag, h.Get("ETag"))
		case h.Get("Content-Length") != strconv.FormatInt(seg[1]-seg[0]+1, 10):
			fail("segment %d Content-Length == %d (got %q)", i, seg[1]-seg[0]+1, h.Get("Content-Length"))
		case !bytes.Equal(b, payload[seg[0]:seg[1]+1]):
			fail("segment %d bytes mismatch", i)
		default:
			reassembled = append(reassembled, b...)
			continue
		}
		segOK = false
	}
	if segOK {
		pass("all %d segments returned 206 with consistent ETag/Content-Length/Content-Range", len(segments))
	}
	check(bytes.Equal(reassembled, payload) && sha256Hex(reassembled) == sha,
		"reassembled artifact is byte-identical to the published one")

	// ---- open-ended range --------------------------------------------------
	mid := size / 2
	status, headers, body = get(base, "/api/firmware/releases/"+version+"/artifact",
		map[string]string{"Range": fmt.Sprintf("bytes=%d-", mid)})
	check(status == http.StatusPartialContent, "open-ended range -> 206 (got %d)", status)
	check(headers.Get("Content-Range") == fmt.Sprintf("bytes %d-%d/%d", mid, size-1, size),
		"open-ended Content-Range (got %q)", headers.Get("Content-Range"))
	check(bytes.Equal(body, payload[mid:]), "open-ended range bytes match")

	// ---- suffix range --------------------------------------------------------
	const tail = 1000
	status, headers, body = get(base, "/api/firmware/releases/"+version+"/artifact",
		map[string]string{"Range": fmt.Sprintf("bytes=-%d", tail)})
	check(status == http.StatusPartialContent, "suffix range -> 206 (got %d)", status)
	check(headers.Get("Content-Range") == fmt.Sprintf("bytes %d-%d/%d", size-tail, size-1, size),
		"suffix Content-Range (got %q)", headers.Get("Content-Range"))
	check(bytes.Equal(body, payload[size-tail:]), "suffix range bytes match")

	// A suffix longer than the whole file yields the whole representation.
	status, headers, body = get(base, "/api/firmware/releases/"+version+"/artifact",
		map[string]string{"Range": fmt.Sprintf("bytes=-%d", size+500)})
	check(status == http.StatusPartialContent, "oversized suffix -> 206 (got %d)", status)
	check(headers.Get("Content-Range") == fmt.Sprintf("bytes 0-%d/%d", size-1, size),
		"oversized suffix Content-Range (got %q)", headers.Get("Content-Range"))
	check(bytes.Equal(body, payload), "oversized suffix returns the whole file")

	// ---- unsatisfiable / malformed ranges ------------------------------------
	wantCR := fmt.Sprintf("bytes */%d", size)
	for _, r := range []string{
		fmt.Sprintf("bytes=%d-", size),           // starts past EOF
		fmt.Sprintf("bytes=%d-%d", size, size+9), // fully past EOF
		"bytes=100-10",                           // start > end
		"bytes=-0",                               // zero-length suffix
		"bytes=abc-def",                          // garbage
		"bytes=",                                 // empty
	} {
		status, headers, _ = get(base, "/api/firmware/releases/"+version+"/artifact",
			map[string]string{"Range": r})
		check(status == http.StatusRequestedRangeNotSatisfiable, "range %q -> 416 (got %d)", r, status)
		check(headers.Get("Content-Range") == wantCR,
			"range %q Content-Range == %q (got %q)", r, wantCR, headers.Get("Content-Range"))
	}

	// ---- If-Range -------------------------------------------------------------
	status, _, body = get(base, "/api/firmware/releases/"+version+"/artifact", map[string]string{
		"Range":    "bytes=0-99",
		"If-Range": etag,
	})
	check(status == http.StatusPartialContent && len(body) == 100,
		"If-Range with current ETag -> 206 with the 100 requested bytes (got %d, %d bytes)", status, len(body))

	status, headers, body = get(base, "/api/firmware/releases/"+version+"/artifact", map[string]string{
		"Range":    "bytes=0-99",
		"If-Range": `"0000000000000000000000000000000000000000000000000000000000000000"`,
	})
	check(status == http.StatusOK, "If-Range with stale ETag -> 200 (got %d)", status)
	check(bytes.Equal(body, payload), "If-Range mismatch returns the complete artifact")
	check(headers.Get("Content-Length") == strconv.FormatInt(size, 10),
		"If-Range mismatch Content-Length == %d", size)

	// ---- duplicate publish did not corrupt the stored bytes -------------------
	status, _, body = get(base, "/api/firmware/releases/"+version+"/artifact",
		map[string]string{"Range": "bytes=0-99"})
	check(status == http.StatusPartialContent && bytes.Equal(body, payload[:100]),
		"original bytes intact after duplicate publish attempt")

	// ---- result -----------------------------------------------------------------
	if failures > 0 {
		fmt.Printf("SMOKE FAILED: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("SMOKE OK")
}

func waitHealthy(base string) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				return
			}
		}
		if time.Now().After(deadline) {
			fmt.Println("smoke: service did not become healthy in time")
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
}

func makePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>13)
	}
	return b
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// splitPlan cuts [0,size) into uneven contiguous closed ranges.
func splitPlan(size int64) [][2]int64 {
	cuts := []int64{0}
	for _, c := range []int64{1, 4097, 65536, 65537, 1 << 20, 1<<20 + 777, size - 500} {
		if c > 0 && c < size {
			cuts = append(cuts, c)
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	cuts = append(cuts, size)
	var segs [][2]int64
	for i := 0; i+1 < len(cuts); i++ {
		segs = append(segs, [2]int64{cuts[i], cuts[i+1] - 1})
	}
	return segs
}

func uploadRelease(base, version, model, sha string, payload []byte) (int, http.Header, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("version", version)
	_ = mw.WriteField("targetModel", model)
	_ = mw.WriteField("sha256", sha)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="artifact"; filename="firmware.bin"`)
	h.Set("Content-Type", "application/octet-stream")
	pw, err := mw.CreatePart(h)
	if err != nil {
		fail("build multipart: %v", err)
		return 0, nil, nil
	}
	if _, err := pw.Write(payload); err != nil {
		fail("build multipart: %v", err)
		return 0, nil, nil
	}
	if err := mw.Close(); err != nil {
		fail("build multipart: %v", err)
		return 0, nil, nil
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/firmware/releases", &buf)
	if err != nil {
		fail("build request: %v", err)
		return 0, nil, nil
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(req)
}

func get(base, path string, headers map[string]string) (int, http.Header, []byte) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		fail("build request: %v", err)
		return 0, nil, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(req)
}

func doRequest(method, url string, headers map[string]string) (int, http.Header, []byte) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		fail("build request: %v", err)
		return 0, nil, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(req)
}

func do(req *http.Request) (int, http.Header, []byte) {
	resp, err := client.Do(req)
	if err != nil {
		fail("%s %s: %v", req.Method, req.URL, err)
		return 0, nil, nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fail("%s %s: read body: %v", req.Method, req.URL, err)
		return 0, nil, nil
	}
	return resp.StatusCode, resp.Header, b
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
