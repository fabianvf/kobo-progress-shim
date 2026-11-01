package capture

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxCapturedBody = 16 << 20

// NewProxy returns a reverse proxy to upstream that reports each exchange to
// recorder. Host, query and X-Forwarded-* are kept as received because CWA
// builds absolute URLs from them.
func NewProxy(upstream *url.URL, recorder *Recorder) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true // never add an Accept-Encoding the Kobo didn't send

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = upstream.Scheme
			r.Out.URL.Host = upstream.Host
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.Out.Host = r.In.Host
			for name, values := range r.In.Header {
				if strings.HasPrefix(name, "X-Forwarded-") || name == "Forwarded" {
					r.Out.Header[name] = values
				}
			}
		},
		Transport:     transport,
		FlushInterval: -1,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		exchange := &Exchange{
			Start:         start,
			Method:        r.Method,
			RequestURI:    r.RequestURI,
			RequestHeader: r.Header.Clone(),
			RequestBody:   &cappedBuffer{limit: maxCapturedBody},
			ResponseBody:  &cappedBuffer{limit: maxCapturedBody},
			RequestSize:   r.ContentLength,
		}
		if r.Body != nil && r.Body != http.NoBody && isJSON(r.Header) {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(r.Body, exchange.RequestBody), r.Body}
		}

		recording := &recordingWriter{ResponseWriter: w, exchange: exchange}
		defer func() {
			exchange.Duration = time.Since(start)
			if exchange.Status == 0 {
				exchange.Status = http.StatusOK
			}
			exchange.ResponseHeader = w.Header().Clone()
			recorder.Record(exchange)
		}()
		proxy.ServeHTTP(recording, r)
	})
}

func isJSON(header http.Header) bool {
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "json")
}

// recordingWriter copies JSON response bodies aside. The Kobo's write always
// goes first and its result is what the proxy sees.
type recordingWriter struct {
	http.ResponseWriter
	exchange *Exchange
	capture  bool
}

func (w *recordingWriter) WriteHeader(status int) {
	if status >= 200 && w.exchange.Status == 0 {
		w.exchange.Status = status
		w.capture = isJSON(w.Header())
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.exchange.Status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.exchange.ResponseSize += int64(n)
	if w.capture {
		w.exchange.ResponseBody.Write(p[:n])
	}
	return n, err
}

func (w *recordingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *recordingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// cappedBuffer keeps the first limit bytes and never returns an error, so it
// is safe on the far side of a TeeReader.
//
// The transport can still be reading a request body after the handler returns,
// so writes can race with the recorder's snapshot.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// snapshot returns a copy of the captured bytes and whether any were cut off.
func (b *cappedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes()), b.truncated
}
