# GCS Proxy

An HTTP server that proxies GET and HEAD requests to Google Cloud Storage bucket objects, with byte ranges and conditional requests, preserving cache headers and returning generic error messages.

## Features

- Proxies HTTP GET and HEAD requests to GCS bucket objects
- Byte-range requests (`Range`, answered with `206 Partial Content`), which video playback in Safari and on iOS requires
- Conditional requests (`If-None-Match`, `If-Modified-Since`, `If-Match`, `If-Unmodified-Since`, `If-Range`)
- Preserves the `Cache-Control`, `Content-Type`, `ETag` and `Last-Modified` headers from GCS
- Authenticates with Application Default Credentials, or sends anonymous requests to public buckets and emulators
- Returns original HTTP status codes (404, 403, etc.) with generic error messages
- Access logging for all HTTP requests with timing and status information
- Configurable via command-line flags or environment variables
- Built with Cobra + Viper for flexible configuration

## Requirements

- Google Cloud credentials with read access to the bucket (`storage.objects.get`, for example the `roles/storage.objectViewer` role), unless the bucket is public

## How to build

On Mac and Linux install tools using [mise](https://mise.jdx.dev/):

```sh
mise install
make download
make build
```

On Windows:

```powershell
winget install GoLang.Go
winget install goreleaser.goreleaser
./Makefile.ps1 download
./Makefile.ps1 build
```

## How to verify a release

Every released binary is published with an SPDX SBOM
(`<binary>.sbom.json`) and a build provenance attestation. Both the binaries
and the SBOMs can be verified with the GitHub CLI:

```sh
gh attestation verify gcs-proxy-linux-amd64 --repo dex4er/gcs-proxy
```

Note that `gh attestation verify` prints its result only on a terminal: in CI
or when its output is piped it reports through the exit status alone. Use
`--format json` when a script needs the details.

The current vulnerability reports are in
[Security scan for main branch](https://github.com/dex4er/gcs-proxy/discussions/2)
and
[Security scan for the last release](https://github.com/dex4er/gcs-proxy/discussions/1).

## Usage

### Command Line

```bash
# Basic usage
./gcs-proxy --bucket my-bucket

# Custom port
./gcs-proxy --bucket my-bucket --port 3000

# With debug logging
./gcs-proxy --bucket my-bucket --loglevel debug

# A public bucket, without credentials
./gcs-proxy --bucket my-public-bucket --anonymous
```

### Environment Variables

Environment variables use the `GCSPROXY_` prefix:

```bash
export GCSPROXY_BUCKET=my-bucket
export GCSPROXY_PORT=8080
export GCSPROXY_LOGLEVEL=info
./gcs-proxy
```

### Command-Line Flags

| Flag          | Environment Variable | Default                          | Description                                    |
|---------------|----------------------|----------------------------------|------------------------------------------------|
| `--bucket`    | `GCSPROXY_BUCKET`    | (required)                       | GCS bucket name                                |
| `--port`      | `GCSPROXY_PORT`      | `8080`                           | HTTP server port                               |
| `--loglevel`  | `GCSPROXY_LOGLEVEL`  | `info`                           | Log level (debug, info, warn, error)           |
| `--endpoint`  | `GCSPROXY_ENDPOINT`  | `https://storage.googleapis.com` | GCS XML API endpoint URL, for emulators        |
| `--anonymous` | `GCSPROXY_ANONYMOUS` | `false`                          | Send requests without credentials              |

## Google Cloud Credentials

The application uses Application Default Credentials, which checks:

1. The `GOOGLE_APPLICATION_CREDENTIALS` environment variable, pointing to a credentials file
2. The credentials of `gcloud auth application-default login`
3. The attached service account (when running on GKE with Workload Identity, Cloud Run, or Compute Engine)

Example:

```bash
gcloud auth application-default login
./gcs-proxy --bucket my-bucket
```

## Example Requests

```bash
# Start the server
./gcs-proxy --bucket my-bucket

# Request an object
curl http://localhost:8080/path/to/object.txt

# Request with path
curl http://localhost:8080/images/logo.png

# Request the first two bytes of a video, as Safari does before playing it
curl -r 0-1 http://localhost:8080/stories/story.mp4

# Request only the headers
curl -I http://localhost:8080/stories/story.mp4
```

## Error Handling

The server returns original HTTP status codes with generic error messages:

- **404 Not Found**: "The requested resource was not found"
- **403 Forbidden**: "Access to the requested resource is forbidden"
- **405 Method Not Allowed**: "Method not allowed", for anything but GET and HEAD
- **412 Precondition Failed**: "The precondition given in the request failed"
- **416 Range Not Satisfiable**: "The requested range is not satisfiable", with `Content-Range: bytes */<size>`
- **500 Internal Server Error**: "An error occurred while processing your request"

A conditional request whose object has not changed gets `304 Not Modified` with no body.

## Access Logging

All HTTP requests are logged with the following information:

- HTTP method
- Request path
- Remote address (client IP)
- HTTP status code
- Request duration in milliseconds
- User agent

Example access log output:

```text
time=2026-10-13T... level=DEBUG msg="HTTP request" method=GET path=/index.html remote_addr=127.0.0.1:52301 status=200 duration_ms=45 user_agent="Mozilla/5.0..."
time=2026-10-13T... level=DEBUG msg="HTTP request" method=GET path=/missing.txt remote_addr=127.0.0.1:52302 status=404 duration_ms=12 user_agent="curl/8.7.1"
```

## Development

```bash
# Run without building
go run . --bucket my-bucket

# Build
make build

# Run tests
make test

# Run the tests against fake-gcs-server too
docker run -d --rm --name fake-gcs-server -p 4443:4443 \
  fsouza/fake-gcs-server:1.56.1 -scheme http -backend memory -public-host localhost:4443
curl -X POST -H "Content-Type: application/json" -d '{"name":"test-bucket"}' \
  "http://localhost:4443/storage/v1/b?project=test"
GCSPROXY_TEST_ENDPOINT=http://localhost:4443 make test
```

## License

MIT
