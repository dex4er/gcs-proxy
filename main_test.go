package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testBody = "0123456789"
	testETag = `"0123abcd"`
)

var testLastModified = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fakeGCS records the requests the proxy makes and answers them with the
// handler a test sets.
type fakeGCS struct {
	handler  http.HandlerFunc
	mu       sync.Mutex
	requests []*http.Request
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.mu.Unlock()
	f.handler(w, r)
}

// sent returns the requests made with the method.
func (f *fakeGCS) sent(method string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var requests []*http.Request
	for _, r := range f.requests {
		if r.Method == method {
			requests = append(requests, r)
		}
	}
	return requests
}

func useFake(t *testing.T, f *fakeGCS) {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	useEndpoint(t, server.URL, "test-bucket", server.Client())
}

func useEndpoint(t *testing.T, url, bucket string, client *http.Client) {
	t.Helper()
	savedEndpoint, savedBucket, savedClient, savedLogger := endpoint, bucketName, httpClient, logger
	endpoint, bucketName, httpClient, logger = url, bucket, client, slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() {
		endpoint, bucketName, httpClient, logger = savedEndpoint, savedBucket, savedClient, savedLogger
	})
}

// object answers like GCS for testBody: the whole object, a single range,
// or 416, and 304 or 412 for the conditional headers.
func object(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("ETag", testETag)
	http.ServeContent(w, r, "", testLastModified, strings.NewReader(testBody))
}

// gcsErrorHandler answers with the status and, unless the code is empty, an XML
// error body like the ones GCS sends.
func gcsErrorHandler(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if code == "" || r.Method == http.MethodHead {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>"+code+"</Code><Message>"+code+"</Message></Error>")
	}
}

func serve(method, path string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleRequest(w, r)
	return w
}

func assertResponse(t *testing.T, w *httptest.ResponseRecorder, status int, body string, header map[string]string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("status = %d, want %d", w.Code, status)
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	for k, want := range header {
		if got := w.Header().Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
}

func TestGetWholeObject(t *testing.T) {
	f := &fakeGCS{handler: object}
	useFake(t, f)

	w := serve(http.MethodGet, "/stories/story.mp4", nil)

	assertResponse(t, w, http.StatusOK, testBody, map[string]string{
		"Accept-Ranges":  "bytes",
		"Cache-Control":  "public, max-age=3600",
		"Content-Type":   "video/mp4",
		"Content-Length": "10",
		"ETag":           testETag,
		"Last-Modified":  "Tue, 13 Oct 2026 12:00:00 GMT",
		"Content-Range":  "",
	})
	gets := f.sent(http.MethodGet)
	if len(gets) != 1 || gets[0].URL.Path != "/test-bucket/stories/story.mp4" || gets[0].Header.Get("Range") != "" {
		t.Errorf("unexpected GET requests: %+v", gets)
	}
	if got := gets[0].Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding sent to GCS = %q, want identity", got)
	}
}

func TestObjectNameEscaping(t *testing.T) {
	f := &fakeGCS{handler: object}
	useFake(t, f)

	serve(http.MethodGet, "/stories/my%20story+1%25.mp4", nil)

	if got := f.sent(http.MethodGet)[0].RequestURI; got != "/test-bucket/stories/my%20story%2B1%25.mp4" {
		t.Errorf("request URI = %q", got)
	}
}

func TestGetRange(t *testing.T) {
	f := &fakeGCS{handler: object}
	useFake(t, f)

	w := serve(http.MethodGet, "/story.mp4", map[string]string{"Range": "bytes=2-4"})

	assertResponse(t, w, http.StatusPartialContent, "234", map[string]string{
		"Accept-Ranges":  "bytes",
		"Content-Range":  "bytes 2-4/10",
		"Content-Length": "3",
	})
	if got := f.sent(http.MethodGet)[0].Header.Get("Range"); got != "bytes=2-4" {
		t.Errorf("Range sent to GCS = %q", got)
	}
}

func TestRangeSentToGCS(t *testing.T) {
	tests := []struct {
		rangeHeader string
		want        string
	}{
		{"bytes=0-1", "bytes=0-1"},
		{"bytes=100-", "bytes=100-"},
		{"bytes=-500", "bytes=-500"},
		{"bytes=0-1,5-9", ""},
		{"bytes=5-2", ""},
		{"bytes=abc", ""},
		{"items=0-1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.rangeHeader, func(t *testing.T) {
			f := &fakeGCS{handler: object}
			useFake(t, f)

			serve(http.MethodGet, "/story.mp4", map[string]string{"Range": tt.rangeHeader})

			if got := f.sent(http.MethodGet)[0].Header.Get("Range"); got != tt.want {
				t.Errorf("Range sent to GCS = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHead(t *testing.T) {
	f := &fakeGCS{handler: object}
	useFake(t, f)

	w := serve(http.MethodHead, "/story.mp4", nil)

	assertResponse(t, w, http.StatusOK, "", map[string]string{
		"Accept-Ranges":  "bytes",
		"Content-Length": "10",
		"ETag":           testETag,
		"Last-Modified":  "Tue, 13 Oct 2026 12:00:00 GMT",
	})
	if heads, gets := f.sent(http.MethodHead), f.sent(http.MethodGet); len(heads) != 1 || len(gets) != 0 {
		t.Errorf("HEAD requests = %d, GET requests = %d", len(heads), len(gets))
	}
}

func TestHeadMissingObject(t *testing.T) {
	// HEAD errors have no body, so there is no "NoSuchKey" code.
	useFake(t, &fakeGCS{handler: gcsErrorHandler(http.StatusNotFound, "NoSuchKey")})

	w := serve(http.MethodHead, "/missing.mp4", nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	useFake(t, &fakeGCS{})

	w := serve(http.MethodPost, "/story.mp4", nil)

	assertResponse(t, w, http.StatusMethodNotAllowed, "Method not allowed\n", map[string]string{"Allow": "GET, HEAD"})
}

func TestEmptyKey(t *testing.T) {
	useFake(t, &fakeGCS{})

	w := serve(http.MethodGet, "/", nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPreconditionsSentToGCS(t *testing.T) {
	date := "Tue, 13 Oct 2026 12:00:00 GMT"
	tests := []struct {
		name              string
		header            map[string]string
		ifMatch           string
		ifNoneMatch       string
		ifModifiedSince   string
		ifUnmodifiedSince string
	}{
		{"If-Match", map[string]string{"If-Match": testETag}, testETag, "", "", ""},
		{"If-None-Match", map[string]string{"If-None-Match": testETag}, "", testETag, "", ""},
		{"If-Modified-Since", map[string]string{"If-Modified-Since": date}, "", "", date, ""},
		{"If-Unmodified-Since", map[string]string{"If-Unmodified-Since": date}, "", "", "", date},
		{"obsolete date format", map[string]string{"If-Modified-Since": "Tuesday, 13-Oct-26 12:00:00 GMT"}, "", "", date, ""},
		{"If-None-Match wins over If-Modified-Since", map[string]string{"If-None-Match": testETag, "If-Modified-Since": date}, "", testETag, "", ""},
		{"If-Match wins over If-Unmodified-Since", map[string]string{"If-Match": testETag, "If-Unmodified-Since": date}, testETag, "", "", ""},
		{"invalid date is ignored", map[string]string{"If-Modified-Since": "yesterday"}, "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeGCS{handler: object}
			useFake(t, f)

			serve(http.MethodGet, "/story.mp4", tt.header)
			serve(http.MethodHead, "/story.mp4", tt.header)

			for _, r := range []*http.Request{f.sent(http.MethodGet)[0], f.sent(http.MethodHead)[0]} {
				for name, want := range map[string]string{
					"If-Match":            tt.ifMatch,
					"If-None-Match":       tt.ifNoneMatch,
					"If-Modified-Since":   tt.ifModifiedSince,
					"If-Unmodified-Since": tt.ifUnmodifiedSince,
				} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("%s %s = %q, want %q", r.Method, name, got, want)
					}
				}
			}
		})
	}
}

func TestNotModified(t *testing.T) {
	useFake(t, &fakeGCS{handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", testETag)
		w.Header().Set("Last-Modified", "Tue, 13 Oct 2026 12:00:00 GMT")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusNotModified)
	}})

	w := serve(http.MethodGet, "/story.mp4", map[string]string{"If-None-Match": testETag})

	assertResponse(t, w, http.StatusNotModified, "", map[string]string{
		"ETag":          testETag,
		"Last-Modified": "Tue, 13 Oct 2026 12:00:00 GMT",
		"Cache-Control": "public, max-age=3600",
	})
}

func TestPreconditionFailed(t *testing.T) {
	useFake(t, &fakeGCS{handler: object})

	w := serve(http.MethodGet, "/story.mp4", map[string]string{"If-Match": `"another"`})

	assertResponse(t, w, http.StatusPreconditionFailed, "The precondition given in the request failed\n", nil)
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  int
		body    string
	}{
		{"precondition failed", gcsErrorHandler(http.StatusPreconditionFailed, "PreconditionFailed"), http.StatusPreconditionFailed, "The precondition given in the request failed\n"},
		{"no such key", gcsErrorHandler(http.StatusNotFound, "NoSuchKey"), http.StatusNotFound, "The requested resource was not found\n"},
		{"not found without a code", gcsErrorHandler(http.StatusNotFound, ""), http.StatusNotFound, "The requested resource was not found\n"},
		{"no such bucket", gcsErrorHandler(http.StatusNotFound, "NoSuchBucket"), http.StatusNotFound, "The requested source does not exist\n"},
		{"access denied", gcsErrorHandler(http.StatusForbidden, "AccessDenied"), http.StatusForbidden, "Access to the requested resource is forbidden\n"},
		{"forbidden without a code", gcsErrorHandler(http.StatusForbidden, ""), http.StatusForbidden, "Access to the requested resource is forbidden\n"},
		{"credentials refused", gcsErrorHandler(http.StatusUnauthorized, "AuthenticationRequired"), http.StatusForbidden, "Access to the requested resource is forbidden\n"},
		{"server error", gcsErrorHandler(http.StatusInternalServerError, "InternalError"), http.StatusInternalServerError, "An error occurred while processing your request\n"},
		{"not an XML body", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		}, http.StatusInternalServerError, "An error occurred while processing your request\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFake(t, &fakeGCS{handler: tt.handler})

			w := serve(http.MethodGet, "/story.mp4", nil)

			assertResponse(t, w, tt.status, tt.body, nil)
		})
	}
}

func TestConnectionError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	useEndpoint(t, server.URL, "test-bucket", &http.Client{})

	w := serve(http.MethodGet, "/story.mp4", nil)

	assertResponse(t, w, http.StatusInternalServerError, "An error occurred while processing your request\n", nil)
}

func TestRangeNotSatisfiable(t *testing.T) {
	f := &fakeGCS{handler: object}
	useFake(t, f)

	w := serve(http.MethodGet, "/story.mp4", map[string]string{"Range": "bytes=100-"})

	assertResponse(t, w, http.StatusRequestedRangeNotSatisfiable, "The requested range is not satisfiable\n", map[string]string{
		"Content-Range": "bytes */10",
	})
	if heads := f.sent(http.MethodHead); len(heads) != 1 || heads[0].Header.Get("Range") != "" {
		t.Errorf("unexpected HEAD requests: %+v", heads)
	}
}

func TestRangeNotSatisfiableWithStaleIfRange(t *testing.T) {
	// If-Range is evaluated before Range: a stale validator gets the whole
	// object even when the range does not fit the current one.
	useFake(t, &fakeGCS{handler: object})

	w := serve(http.MethodGet, "/story.mp4", map[string]string{"Range": "bytes=100-", "If-Range": `"stale"`})

	assertResponse(t, w, http.StatusOK, testBody, nil)
}

func TestIfRange(t *testing.T) {
	tests := []struct {
		name    string
		ifRange string
		status  int
		body    string
		gets    int
	}{
		{"matching ETag", testETag, http.StatusPartialContent, "234", 1},
		{"stale ETag", `"stale"`, http.StatusOK, testBody, 2},
		{"weak ETag", "W/" + testETag, http.StatusOK, testBody, 2},
		{"matching date", "Tue, 13 Oct 2026 12:00:00 GMT", http.StatusPartialContent, "234", 1},
		{"other date", "Tue, 13 Oct 2026 11:59:59 GMT", http.StatusOK, testBody, 2},
		{"invalid value", "yesterday", http.StatusOK, testBody, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeGCS{handler: object}
			useFake(t, f)

			w := serve(http.MethodGet, "/story.mp4", map[string]string{"Range": "bytes=2-4", "If-Range": tt.ifRange})

			assertResponse(t, w, tt.status, tt.body, nil)
			gets := f.sent(http.MethodGet)
			if len(gets) != tt.gets {
				t.Errorf("GET requests = %d, want %d", len(gets), tt.gets)
			}
			for _, r := range gets {
				if r.Header.Get("If-Range") != "" {
					t.Errorf("If-Range sent to GCS: %q", r.Header.Get("If-Range"))
				}
			}
			if tt.gets == 2 && gets[1].Header.Get("Range") != "" {
				t.Errorf("second GET still has Range %q", gets[1].Header.Get("Range"))
			}
		})
	}
}
