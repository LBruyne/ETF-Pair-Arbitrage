// Package livetest supports the gated tests that read the real sources. It
// keeps every HTTP response as it was received, with the request's URL and
// the retrieval time, and writes them to the evidence directory, so that
// what a client parsed can be checked against what the source sent.
package livetest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Dir is the evidence directory, experiments/core_step2 of the repository,
// as seen from a package three levels below the repository root's core/
// directory (core/cex/<source>).
var Dir = filepath.Join("..", "..", "..", "experiments", "core_step2")

// Response is one HTTP exchange as recorded.
type Response struct {
	Method          string `json:"method"`
	URL             string `json:"url"`
	RetrievedAtMs   int64  `json:"retrieved_at_unix_ms"`
	RetrievedAtUTC8 string `json:"retrieved_at_utc8"`
	HTTPStatus      int    `json:"http_status"`
	Body            string `json:"body"` // the response body, byte for byte
}

// Recorder is an http.RoundTripper that passes every request to the default
// transport and keeps the response. A test sets it as its client's
// transport.
type Recorder struct {
	mu        sync.Mutex
	responses []Response
}

// RoundTrip sends the request, reads the whole response body, records it and
// hands the caller an identical body.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	at := time.Now()
	r.mu.Lock()
	r.responses = append(r.responses, Response{Method: req.Method, URL: req.URL.String(), RetrievedAtMs: at.UnixMilli(),
		RetrievedAtUTC8: at.In(time.FixedZone("UTC+8", 8*60*60)).Format(time.DateTime), HTTPStatus: resp.StatusCode, Body: string(body)})
	r.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// Responses returns what was recorded so far, oldest first.
func (r *Recorder) Responses() []Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Response(nil), r.responses...)
}

// Save writes the recorded responses to Dir as <name>_<UTC time>.json and
// returns the file's path.
func (r *Recorder) Save(name string) (string, error) {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(r.Responses(), "", " ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(Dir, name+"_"+time.Now().UTC().Format("20060102T150405Z")+".json")
	return path, os.WriteFile(path, out, 0o644)
}
