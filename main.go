package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/oauth2/google"
)

var version = "dev"

// readOnlyScope is the OAuth scope the proxy asks for: it only reads objects.
const readOnlyScope = "https://www.googleapis.com/auth/devstorage.read_only"

var (
	bucketName    string
	port          string
	logLevel      string
	endpoint      string
	anonymous     bool
	indexDocument string
	errorDocument string
	spa           bool
	httpClient    *http.Client
	logger        *slog.Logger
)

var rootCmd = &cobra.Command{
	Use:     "gcs-proxy",
	Short:   "HTTP proxy server for GCS bucket objects",
	Long:    "An HTTP server that proxies GET and HEAD requests to Google Cloud Storage bucket objects, preserving cache headers and returning generic error messages.",
	Version: version,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServer()
	},
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&bucketName, "bucket", "", "GCS bucket name (required)")
	rootCmd.PersistentFlags().StringVar(&port, "port", "8080", "HTTP server port")
	rootCmd.PersistentFlags().StringVar(&logLevel, "loglevel", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&endpoint, "endpoint", "https://storage.googleapis.com", "GCS XML API endpoint URL")
	rootCmd.PersistentFlags().BoolVar(&anonymous, "anonymous", false, "Send requests without credentials, for public buckets and emulators")
	rootCmd.PersistentFlags().StringVar(&indexDocument, "index-document", "", "Object served for a path ending in a slash, for example index.html")
	rootCmd.PersistentFlags().BoolVar(&spa, "spa", false, "Serve the index document of the bucket root for every path whose last segment has no dot")
	rootCmd.PersistentFlags().StringVar(&errorDocument, "error-document", "", "Object served with 404 for a missing object, for example 404.html")

	if err := viper.BindPFlag("bucket", rootCmd.PersistentFlags().Lookup("bucket")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("port", rootCmd.PersistentFlags().Lookup("port")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("loglevel", rootCmd.PersistentFlags().Lookup("loglevel")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("endpoint", rootCmd.PersistentFlags().Lookup("endpoint")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("anonymous", rootCmd.PersistentFlags().Lookup("anonymous")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("index-document", rootCmd.PersistentFlags().Lookup("index-document")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("spa", rootCmd.PersistentFlags().Lookup("spa")); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("error-document", rootCmd.PersistentFlags().Lookup("error-document")); err != nil {
		panic(err)
	}
}

func initConfig() {
	viper.SetEnvPrefix("GCSPROXY")
	// A flag with a dash in its name is read with an underscore instead.
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
}

func runServer() error {
	bucketName = viper.GetString("bucket")
	port = viper.GetString("port")
	logLevel = viper.GetString("loglevel")
	endpoint = viper.GetString("endpoint")
	anonymous = viper.GetBool("anonymous")
	indexDocument = strings.TrimPrefix(viper.GetString("index-document"), "/")
	spa = viper.GetBool("spa")
	errorDocument = strings.TrimPrefix(viper.GetString("error-document"), "/")

	// Initialize logger
	var level slog.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	logger = slog.New(handler)

	if bucketName == "" {
		return fmt.Errorf("bucket name is required")
	}
	if spa && indexDocument == "" {
		return fmt.Errorf("--spa requires --index-document")
	}

	// Initialize the HTTP client, with Application Default Credentials
	// unless the requests go out anonymously.
	if anonymous {
		httpClient = &http.Client{}
	} else {
		client, err := google.DefaultClient(context.Background(), readOnlyScope)
		if err != nil {
			return fmt.Errorf("failed to load Google credentials: %w", err)
		}
		httpClient = client
	}

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/", loggingMiddleware(handleRequest))

	addr := fmt.Sprintf(":%s", port)
	logger.Info("Starting GCS proxy server",
		"address", addr,
		"bucket", bucketName,
		"endpoint", endpoint,
		"anonymous", anonymous,
		"index_document", indexDocument,
		"spa", spa,
		"error_document", errorDocument,
		"loglevel", logLevel)
	return http.ListenAndServe(addr, nil)
}

// responseWriter wraps http.ResponseWriter to capture the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{w, http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware logs HTTP access information
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := newResponseWriter(w)

		next(wrapped, r)

		duration := time.Since(start)
		logger.Debug("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
			"user_agent", r.UserAgent(),
		)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get the object path from the request URL
	objectKey := indexObject(strings.TrimPrefix(r.URL.Path, "/"))

	// If path is empty, return error
	if objectKey == "" {
		notFound(w, r)
		return
	}

	if r.Method == http.MethodHead {
		headObject(w, r, objectKey)
		return
	}
	getObject(w, r, objectKey)
}

// indexObject returns the object a request path is served from. With an index
// document, a path ending in a slash gets the document appended, the way a
// GCS website does. With --spa, a path whose last segment has no dot is a
// client-side route rather than a file, and gets the index document of the
// bucket root instead.
func indexObject(objectKey string) string {
	if indexDocument == "" {
		return objectKey
	}
	if spa && !strings.Contains(objectKey[strings.LastIndex(objectKey, "/")+1:], ".") {
		return indexDocument
	}
	if objectKey == "" || strings.HasSuffix(objectKey, "/") {
		return objectKey + indexDocument
	}
	return objectKey
}

func headObject(w http.ResponseWriter, r *http.Request, objectKey string) {
	resp, err := gcsRequest(r.Context(), http.MethodHead, objectKey, requestPreconditions(r))
	if err != nil {
		handleObjectError(w, r, objectKey, err)
		return
	}
	defer closeBody(resp.Body)

	setObjectHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
}

func getObject(w http.ResponseWriter, r *http.Request, objectKey string) {
	header := requestPreconditions(r)
	if byteRange := requestRange(r); byteRange != "" {
		header.Set("Range", byteRange)
	}
	ifRange := r.Header.Get("If-Range")

	resp, err := gcsRequest(r.Context(), http.MethodGet, objectKey, header)

	// The part is only valid if the object is still the one the client got
	// its validator from. Checking the response rather than asking first
	// keeps the common case to one request and leaves no window for the
	// object to change in between.
	if err == nil && header.Get("Range") != "" && ifRange != "" && !ifRangeMatches(ifRange, resp.Header) {
		closeBody(resp.Body)
		header.Del("Range")
		resp, err = gcsRequest(r.Context(), http.MethodGet, objectKey, header)
	}

	if err != nil {
		if header.Get("Range") != "" && isInvalidRange(err) {
			rangeNotSatisfiable(w, r, objectKey, header, ifRange)
			return
		}
		handleObjectError(w, r, objectKey, err)
		return
	}

	writeObject(w, resp)
}

// rangeNotSatisfiable answers a range that starts past the end of the
// object. The 416 has to carry the object size and If-Range needs the
// validators of the object, so both come from a HEAD rather than from
// whatever the error response happens to include. If-Range is evaluated
// before Range, so a stale validator still gets the whole object instead.
func rangeNotSatisfiable(w http.ResponseWriter, r *http.Request, objectKey string, header http.Header, ifRange string) {
	header.Del("Range")
	head, err := gcsRequest(r.Context(), http.MethodHead, objectKey, header)
	if err != nil {
		handleObjectError(w, r, objectKey, err)
		return
	}
	closeBody(head.Body)

	if ifRange != "" && !ifRangeMatches(ifRange, head.Header) {
		resp, err := gcsRequest(r.Context(), http.MethodGet, objectKey, header)
		if err != nil {
			handleObjectError(w, r, objectKey, err)
			return
		}
		writeObject(w, resp)
		return
	}

	if size := head.Header.Get("Content-Length"); size != "" {
		w.Header().Set("Content-Range", "bytes */"+size)
	}
	http.Error(w, "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
}

func writeObject(w http.ResponseWriter, resp *http.Response) {
	defer closeBody(resp.Body)

	setObjectHeaders(w.Header(), resp.Header)

	status := http.StatusOK
	if contentRange := resp.Header.Get("Content-Range"); contentRange != "" {
		w.Header().Set("Content-Range", contentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	// Copy the GCS object body to response
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Error("Error writing response", "error", err)
	}
}

func closeBody(body io.ReadCloser) {
	if err := body.Close(); err != nil {
		logger.Error("Error closing response body", "error", err)
	}
}

// objectHeaders are the object metadata a client needs to cache the object
// and to ask for parts of it.
var objectHeaders = []string{"Cache-Control", "Content-Type", "Content-Length", "ETag", "Last-Modified"}

// setObjectHeaders copies the object headers from the GCS response.
func setObjectHeaders(dst, src http.Header) {
	dst.Set("Accept-Ranges", "bytes")
	for _, name := range objectHeaders {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}

// objectURL is the XML API URL of an object, path-style. The object name is
// escaped segment by segment, so that its slashes stay path separators and
// anything else outside the unreserved characters, "+" included, is
// percent-encoded.
func objectURL(objectKey string) string {
	segments := strings.Split(objectKey, "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return strings.TrimSuffix(endpoint, "/") + "/" + url.PathEscape(bucketName) + "/" + strings.Join(segments, "/")
}

// gcsError is a GCS response with a status other than 2xx. HEAD responses
// have no body, so their code and message are empty.
type gcsError struct {
	status  int
	header  http.Header
	code    string
	message string
}

func (e *gcsError) Error() string {
	return fmt.Sprintf("GCS responded %d %s: %s", e.status, e.code, e.message)
}

// gcsRequest sends a request for the object to GCS. A response with a status
// other than 2xx is returned as a *gcsError, with its body read and closed.
func gcsRequest(ctx context.Context, method, objectKey string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, objectURL(objectKey), nil)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	// Asking for gzip would make the transport decompress the response on
	// its own and drop its Content-Length, and GCS would send an object
	// stored gzip-compressed as it is, to a client that may not accept it.
	// With identity, GCS decompresses such an object itself.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	defer closeBody(resp.Body)
	ge := &gcsError{status: resp.StatusCode, header: resp.Header, message: resp.Status}
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) == nil {
		ge.code = body.Code
		if body.Message != "" {
			ge.message = body.Message
		}
	}
	return nil, ge
}

// singleRange matches one byte range: "bytes=first-[last]" or "bytes=-suffix".
var singleRange = regexp.MustCompile(`^bytes=(?:(\d+)-(\d*)|-\d+)$`)

// requestRange returns the Range header when it asks for a single byte
// range, and "" otherwise. GCS serves one range per request, and RFC 9110
// lets a server ignore a Range it does not support or cannot parse, which
// then gets the whole object with 200.
func requestRange(r *http.Request) string {
	value := r.Header.Get("Range")
	m := singleRange.FindStringSubmatch(value)
	if m == nil {
		return ""
	}
	if m[1] != "" && m[2] != "" {
		first, errFirst := strconv.ParseInt(m[1], 10, 64)
		last, errLast := strconv.ParseInt(m[2], 10, 64)
		if errFirst != nil || errLast != nil || last < first {
			return ""
		}
	}
	return value
}

// requestPreconditions returns the conditional request headers GCS
// evaluates itself. As RFC 9110 requires, a date condition is dropped when
// the ETag condition it pairs with is present, and an invalid date is
// ignored.
func requestPreconditions(r *http.Request) http.Header {
	h := http.Header{}
	if v := r.Header.Get("If-Match"); v != "" {
		h.Set("If-Match", v)
	} else if v := headerTime(r, "If-Unmodified-Since"); v != "" {
		h.Set("If-Unmodified-Since", v)
	}
	if v := r.Header.Get("If-None-Match"); v != "" {
		h.Set("If-None-Match", v)
	} else if v := headerTime(r, "If-Modified-Since"); v != "" {
		h.Set("If-Modified-Since", v)
	}
	return h
}

// headerTime returns the date in the header in the preferred HTTP-date
// format, or "" when it is not a valid date.
func headerTime(r *http.Request, name string) string {
	t, err := http.ParseTime(r.Header.Get(name))
	if err != nil {
		return ""
	}
	return t.UTC().Format(http.TimeFormat)
}

// ifRangeMatches evaluates If-Range against the object GCS returned. GCS
// has no If-Range, so the proxy does it: a strong ETag must match exactly
// and an HTTP-date must equal Last-Modified. Anything else, a weak ETag
// included, does not match, and the client gets the whole object.
func ifRangeMatches(ifRange string, object http.Header) bool {
	if strings.HasPrefix(ifRange, `"`) {
		return object.Get("ETag") == ifRange
	}
	if strings.HasPrefix(ifRange, "W/") {
		return false
	}
	t, err := http.ParseTime(ifRange)
	if err != nil {
		return false
	}
	lastModified, err := http.ParseTime(object.Get("Last-Modified"))
	return err == nil && lastModified.Equal(t)
}

// handleObjectError answers a GCS error for the object. A missing object gets
// the error document, when there is one and it is not the missing object
// itself.
func handleObjectError(w http.ResponseWriter, r *http.Request, objectKey string, err error) {
	if errorDocument != "" && objectKey != errorDocument && isNoSuchKey(err) {
		notFound(w, r)
		return
	}
	handleGCSError(w, err)
}

// notFound answers 404, with the error document as the body when there is
// one. Only its type and length go to the client: its validators and
// caching headers describe the document, not the missing object.
func notFound(w http.ResponseWriter, r *http.Request) {
	if errorDocument != "" {
		resp, err := gcsRequest(r.Context(), r.Method, errorDocument, http.Header{})
		if err == nil {
			defer closeBody(resp.Body)
			for _, name := range []string{"Content-Type", "Content-Length"} {
				if v := resp.Header.Get(name); v != "" {
					w.Header().Set(name, v)
				}
			}
			w.WriteHeader(http.StatusNotFound)
			if _, err := io.Copy(w, resp.Body); err != nil {
				logger.Error("Error writing response", "error", err)
			}
			return
		}
		logger.Error("Error document not available", "error", err)
	}
	http.Error(w, "The requested resource was not found", http.StatusNotFound)
}

// isNoSuchKey tells a missing object from a missing bucket, which has a code
// of its own. A HEAD error has no code at all.
func isNoSuchKey(err error) bool {
	var ge *gcsError
	return errors.As(err, &ge) && (ge.code == "NoSuchKey" || ge.status == http.StatusNotFound && ge.code == "")
}

func isInvalidRange(err error) bool {
	var ge *gcsError
	return errors.As(err, &ge) && (ge.status == http.StatusRequestedRangeNotSatisfiable || ge.code == "InvalidRange")
}

// handleGCSError maps a GCS error to the client response. The HTTP status
// decides where the code does not: HEAD errors have no body, so they have no
// code at all.
func handleGCSError(w http.ResponseWriter, err error) {
	status, code, message := 0, "", err.Error()
	var ge *gcsError
	if errors.As(err, &ge) {
		status, code, message = ge.status, ge.code, ge.message
	}

	switch {
	case status == http.StatusNotModified:
		// A 304 carries the validators and caching headers a 200 would.
		for _, name := range []string{"Cache-Control", "ETag", "Last-Modified"} {
			if v := ge.header.Get(name); v != "" {
				w.Header().Set(name, v)
			}
		}
		w.WriteHeader(http.StatusNotModified)
		logger.Debug("GCS object not modified")
		return
	case status == http.StatusPreconditionFailed || code == "PreconditionFailed":
		http.Error(w, "The precondition given in the request failed", http.StatusPreconditionFailed)
	case code == "InvalidBucketName" || code == "NoSuchBucket":
		http.Error(w, "The requested source does not exist", http.StatusNotFound)
	case code == "NoSuchKey" || status == http.StatusNotFound:
		http.Error(w, "The requested resource was not found", http.StatusNotFound)
	// A 401 means GCS refused the proxy's own credentials, which to the
	// client is the same as having no access.
	case code == "AccessDenied" || status == http.StatusUnauthorized || status == http.StatusForbidden:
		http.Error(w, "Access to the requested resource is forbidden", http.StatusForbidden)
	default:
		http.Error(w, "An error occurred while processing your request", http.StatusInternalServerError)
	}
	logger.Error("GCS error", "status", status, "code", code, "message", message)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
