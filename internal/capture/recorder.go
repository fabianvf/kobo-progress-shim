package capture

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queueSize = 256
	// Stays under the volume's sizeLimit, because breaching that evicts the pod
	// and takes the Kobo's path down with it.
	maxCaptureBytes = 512 << 20
)

// Exchange is one request and response, unredacted. It must not leave this
// package except through the redacted Record.
type Exchange struct {
	Start          time.Time
	Duration       time.Duration
	Method         string
	RequestURI     string
	Status         int
	RequestHeader  http.Header
	ResponseHeader http.Header
	RequestBody    *cappedBuffer
	ResponseBody   *cappedBuffer
	RequestSize    int64
	ResponseSize   int64
}

// Record is the redacted form written to the capture file.
type Record struct {
	Time              time.Time           `json:"time"`
	DurationMS        int64               `json:"duration_ms"`
	Method            string              `json:"method"`
	URI               string              `json:"uri"`
	Status            int                 `json:"status"`
	RequestHeaders    map[string][]string `json:"request_headers"`
	ResponseHeaders   map[string][]string `json:"response_headers"`
	RequestBytes      int64               `json:"request_bytes"`
	ResponseBytes     int64               `json:"response_bytes"`
	RequestBody       any                 `json:"request_body,omitempty"`
	ResponseBody      any                 `json:"response_body,omitempty"`
	RequestTruncated  bool                `json:"request_truncated,omitempty"`
	ResponseTruncated bool                `json:"response_truncated,omitempty"`
}

// Recorder writes exchanges from a background goroutine. When the queue is
// full, exchanges are dropped rather than holding up the proxy.
type Recorder struct {
	queue    chan *Exchange
	file     *os.File
	written  int64
	maxBytes int64
	done     sync.WaitGroup
	dropped  atomic.Int64
	closing  sync.Once

	// Observer is optional. Set it before traffic starts.
	Observer Observer
}

// NewRecorder never fails: an unwritable path only loses the capture.
func NewRecorder(path string) *Recorder {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("capture disabled: %v", err)
	}
	r := &Recorder{queue: make(chan *Exchange, queueSize), file: file, maxBytes: maxCaptureBytes}
	if file != nil {
		if info, err := file.Stat(); err == nil {
			r.written = info.Size()
		}
	}
	r.done.Add(1)
	go r.run()
	return r
}

// Record queues an exchange without blocking.
func (r *Recorder) Record(exchange *Exchange) {
	select {
	case r.queue <- exchange:
	default:
		log.Printf("capture queue full, %d exchanges dropped", r.dropped.Add(1))
	}
}

// Close drains the queue. Record must not be called afterwards.
func (r *Recorder) Close() {
	r.closing.Do(func() {
		close(r.queue)
		r.done.Wait()
		if r.file != nil {
			r.file.Close()
		}
	})
}

func (r *Recorder) run() {
	defer r.done.Done()
	for exchange := range r.queue {
		r.observe(exchange)
		r.write(exchange)
	}
}

func (r *Recorder) write(exchange *Exchange) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("capture failed: %T", recovered)
		}
	}()
	if r.file == nil {
		return
	}
	line, err := json.Marshal(redact(exchange))
	if err != nil {
		log.Printf("capture marshal failed: %T", err)
		return
	}
	if r.written+int64(len(line))+1 > r.maxBytes {
		log.Printf("capture size cap reached, %d exchanges dropped", r.dropped.Add(1))
		return
	}
	n, err := r.file.Write(append(line, '\n'))
	r.written += int64(n)
	if err != nil {
		log.Printf("capture write failed: %T", err)
	}
}

func redact(e *Exchange) Record {
	responseBody, responseTruncated := redactBody(e.ResponseBody, e.ResponseHeader)
	requestBody, requestTruncated := redactBody(e.RequestBody, e.RequestHeader)
	return Record{
		Time:              e.Start.UTC(),
		DurationMS:        e.Duration.Milliseconds(),
		Method:            e.Method,
		URI:               redactString(e.RequestURI),
		Status:            e.Status,
		RequestHeaders:    redactHeaders(e.RequestHeader),
		ResponseHeaders:   redactHeaders(e.ResponseHeader),
		RequestBytes:      e.RequestSize,
		ResponseBytes:     e.ResponseSize,
		RequestBody:       requestBody,
		ResponseBody:      responseBody,
		RequestTruncated:  requestTruncated,
		ResponseTruncated: responseTruncated,
	}
}

// redactBody returns the redacted JSON value. A body that can't be parsed is
// dropped instead of logged raw, since it can't be redacted.
func redactBody(body *cappedBuffer, header http.Header) (any, bool) {
	raw, truncated := body.snapshot()
	if len(raw) == 0 {
		return nil, false
	}
	raw, err := decoded(raw, header)
	if err != nil {
		return "[unparseable]", truncated
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "[unparseable]", truncated
	}
	return redactValue(value), truncated
}

func decoded(raw []byte, header http.Header) ([]byte, error) {
	if !strings.EqualFold(header.Get("Content-Encoding"), "gzip") {
		return raw, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(reader, maxCapturedBody))
}
