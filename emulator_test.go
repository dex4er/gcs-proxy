package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"testing"
)

// TestEmulator runs the proxy against a GCS emulator, fake-gcs-server in CI,
// so that what the unit tests assume about GCS responses is checked too. It
// needs GCSPROXY_TEST_ENDPOINT, for example http://localhost:4443, and an
// existing bucket named by GCSPROXY_TEST_BUCKET (default "test-bucket");
// without the endpoint it is skipped. Requests go out without credentials.
func TestEmulator(t *testing.T) {
	testEndpoint := os.Getenv("GCSPROXY_TEST_ENDPOINT")
	if testEndpoint == "" {
		t.Skip("GCSPROXY_TEST_ENDPOINT is not set")
	}
	bucket := os.Getenv("GCSPROXY_TEST_BUCKET")
	if bucket == "" {
		bucket = "test-bucket"
	}

	payload := make([]byte, 300000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// The space checks that the object name is escaped.
	key := "stories/my story.mp4"
	upload(t, testEndpoint, bucket, key, payload)

	useEndpoint(t, testEndpoint, bucket, &http.Client{})
	server := httptest.NewServer(http.HandlerFunc(handleRequest))
	t.Cleanup(server.Close)

	path := "/stories/my%20story.mp4"
	head := request(t, server.URL, http.MethodHead, path, nil)
	etag, lastModified := head.Header.Get("ETag"), head.Header.Get("Last-Modified")
	if etag == "" || lastModified == "" {
		t.Fatalf("HEAD returned %d with no validators: ETag %q, Last-Modified %q", head.StatusCode, etag, lastModified)
	}

	// fake-gcs-server ignores the conditional headers, so the cases that
	// depend on GCS evaluating them run only against an emulator that does.
	conditionals := request(t, server.URL, http.MethodGet, path, map[string]string{"If-None-Match": etag}).StatusCode == http.StatusNotModified

	size := len(payload)
	tests := []struct {
		name         string
		method       string
		path         string
		header       map[string]string
		conditional  bool
		status       int
		body         []byte
		contentRange string
	}{
		{"whole object", http.MethodGet, path, nil, false, http.StatusOK, payload, ""},
		{"first two bytes", http.MethodGet, path, map[string]string{"Range": "bytes=0-1"}, false, http.StatusPartialContent, payload[0:2], fmt.Sprintf("bytes 0-1/%d", size)},
		{"middle range", http.MethodGet, path, map[string]string{"Range": "bytes=1000-1999"}, false, http.StatusPartialContent, payload[1000:2000], fmt.Sprintf("bytes 1000-1999/%d", size)},
		{"suffix range", http.MethodGet, path, map[string]string{"Range": "bytes=-500"}, false, http.StatusPartialContent, payload[size-500:], fmt.Sprintf("bytes %d-%d/%d", size-500, size-1, size)},
		{"open range", http.MethodGet, path, map[string]string{"Range": "bytes=299990-"}, false, http.StatusPartialContent, payload[299990:], fmt.Sprintf("bytes 299990-%d/%d", size-1, size)},
		{"range past the end", http.MethodGet, path, map[string]string{"Range": "bytes=999999-"}, false, http.StatusRequestedRangeNotSatisfiable, nil, fmt.Sprintf("bytes */%d", size)},
		{"multiple ranges", http.MethodGet, path, map[string]string{"Range": "bytes=0-1,5-9"}, false, http.StatusOK, payload, ""},
		{"HEAD", http.MethodHead, path, nil, false, http.StatusOK, nil, ""},
		{"HEAD missing", http.MethodHead, "/stories/missing.mp4", nil, false, http.StatusNotFound, nil, ""},
		{"GET missing", http.MethodGet, "/stories/missing.mp4", nil, false, http.StatusNotFound, nil, ""},
		{"If-None-Match", http.MethodGet, path, map[string]string{"If-None-Match": etag}, true, http.StatusNotModified, nil, ""},
		{"If-None-Match on HEAD", http.MethodHead, path, map[string]string{"If-None-Match": etag}, true, http.StatusNotModified, nil, ""},
		{"If-Modified-Since", http.MethodGet, path, map[string]string{"If-Modified-Since": lastModified}, true, http.StatusNotModified, nil, ""},
		{"If-Match with another ETag", http.MethodGet, path, map[string]string{"If-Match": `"another"`}, true, http.StatusPreconditionFailed, nil, ""},
		{"If-Range matching", http.MethodGet, path, map[string]string{"If-Range": etag, "Range": "bytes=0-1"}, false, http.StatusPartialContent, payload[0:2], fmt.Sprintf("bytes 0-1/%d", size)},
		{"If-Range stale", http.MethodGet, path, map[string]string{"If-Range": `"stale"`, "Range": "bytes=0-1"}, false, http.StatusOK, payload, ""},
		{"If-Range stale past the end", http.MethodGet, path, map[string]string{"If-Range": `"stale"`, "Range": "bytes=999999-"}, false, http.StatusOK, payload, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.conditional && !conditionals {
				t.Skip("the emulator ignores conditional headers")
			}

			resp := request(t, server.URL, tt.method, tt.path, tt.header)

			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if got := resp.Header.Get("Content-Range"); got != tt.contentRange {
				t.Errorf("Content-Range = %q, want %q", got, tt.contentRange)
			}
			if tt.body != nil && !bytes.Equal(resp.body, tt.body) {
				t.Errorf("body is %d bytes, want %d bytes of the object", len(resp.body), len(tt.body))
			}
			if tt.status == http.StatusOK || tt.status == http.StatusPartialContent {
				for _, name := range []string{"Accept-Ranges", "ETag", "Last-Modified", "Content-Type", "Cache-Control"} {
					if resp.Header.Get(name) == "" {
						t.Errorf("no %s header", name)
					}
				}
			}
		})
	}
}

// upload stores the object through the JSON API, which fake-gcs-server
// accepts uploads on, unlike the XML API.
func upload(t *testing.T, base, bucket, key string, payload []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	metadata, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(metadata, `{"name":%q,"contentType":"video/mp4","cacheControl":"public, max-age=3600"}`, key)
	media, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"video/mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = media.Write(payload)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(base+"/upload/storage/v1/b/"+url.PathEscape(bucket)+"/o?uploadType=multipart",
		"multipart/related; boundary="+mw.Boundary(), &body)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %s: %s", resp.Status, message)
	}
}

type response struct {
	*http.Response
	body []byte
}

func request(t *testing.T, base, method, path string, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp, body}
}
