package capture

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "sekrit-value"

func startProxy(t *testing.T, path string, upstream http.HandlerFunc) (*httptest.Server, *Recorder) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder(path)
	t.Cleanup(recorder.Close)
	front := httptest.NewServer(NewProxy(target, recorder))
	t.Cleanup(front.Close)
	return front, recorder
}

func noRedirects() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableCompression: true},
	}
}

func TestForwardsRequestAndResponseUnchanged(t *testing.T) {
	binary := bytes.Repeat([]byte{0, 1, 2, 0xff}, 1<<18)
	front, _ := startProxy(t, filepath.Join(t.TempDir(), "c.jsonl"), func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"a":1}` {
			t.Errorf("body = %q", body)
		}
		if r.Host != "books.example" {
			t.Errorf("host = %q", r.Host)
		}
		if r.Header.Get("X-Forwarded-Host") != "books.example" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("forwarded headers = %v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("authorization not forwarded")
		}
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("accept-encoding added: %q", r.Header.Get("Accept-Encoding"))
		}
		if r.URL.RawQuery != "a=1;b=2" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Header().Set("X-Kobo-Sync", "continue")
		w.Header().Set("Content-Type", "application/epub+zip")
		w.WriteHeader(http.StatusTeapot)
		w.Write(binary)
	})

	req, _ := http.NewRequest(http.MethodPut, front.URL+"/kobo/tok/v1/library/x/state?a=1;b=2", strings.NewReader(`{"a":1}`))
	req.Host = "books.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Forwarded-Host", "books.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := noRedirects().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTeapot || !bytes.Equal(got, binary) || resp.Header.Get("X-Kobo-Sync") != "continue" {
		t.Fatalf("response changed: status %d, %d bytes", resp.StatusCode, len(got))
	}
}

func TestRedirectIsPassedThrough(t *testing.T) {
	front, _ := startProxy(t, filepath.Join(t.TempDir(), "c.jsonl"), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://storeapi.kobo.com/v1/library/x/metadata", http.StatusTemporaryRedirect)
	})
	resp, err := noRedirects().Get(front.URL + "/kobo/tok/v1/library/x/metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "https://storeapi.kobo.com/v1/library/x/metadata" {
		t.Fatalf("redirect changed: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSecretsNeverReachCaptureFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	front, recorder := startProxy(t, path, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Set-Cookie", "session="+secret)
		w.Header().Set("X-Kobo-Apitoken", secret)
		w.Header().Set("X-Kobo-Synctoken", secret)
		w.Header().Set("Location", "/kobo/"+secret+"/next?sig="+secret)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": secret,
			"UserKey":     secret,
			"Nested":      []any{map[string]any{"refresh_token": secret, "Title": "Dune"}},
			"DownloadUrl": "https://books.example/kobo/" + secret + "/download/1/epub",
			"SignedUrl":   "https://cdn.kobo.com/a.epub?sig=" + secret,
		})
	})

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/kobo/"+secret+"/v1/x?key="+secret, strings.NewReader(`{"password":"`+secret+`","Isbn":"123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Cookie", "a="+secret)
	req.Header.Set("X-Kobo-Userkey", secret)
	req.Header.Set("X-Kobo-Synctoken", secret)
	resp, err := noRedirects().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	front.Close()
	recorder.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatalf("secret leaked: %s", data)
	}
	for _, want := range []string{"Dune", "123", "/kobo/[redacted]/download/1/epub", "X-Kobo-Userkey"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("capture missing %q: %s", want, data)
		}
	}
}

func TestGzipBodyIsDecodedForCaptureOnly(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Write([]byte(`[{"Isbn":"9780441013593"}]`))
	gz.Close()

	path := filepath.Join(t.TempDir(), "c.jsonl")
	front, recorder := startProxy(t, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	})
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/library/sync", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := noRedirects().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, compressed.Bytes()) {
		t.Fatal("client did not receive the original gzip bytes")
	}
	front.Close()
	recorder.Close()
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("9780441013593")) {
		t.Fatalf("decoded body missing: %s", data)
	}
}

func TestUnparseableBodyIsNotLoggedRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	front, recorder := startProxy(t, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json ` + secret))
	})
	resp, err := noRedirects().Get(front.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	front.Close()
	recorder.Close()
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(secret)) {
		t.Fatalf("raw body logged: %s", data)
	}
}

func TestUnwritableCaptureStillProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "c.jsonl")
	front, _ := startProxy(t, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	resp, err := noRedirects().Get(front.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestSizeCapStopsWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	r := NewRecorder(path)
	r.maxBytes = 1
	r.write(&Exchange{RequestBody: &cappedBuffer{}, ResponseBody: &cappedBuffer{}})
	r.Close()
	data, _ := os.ReadFile(path)
	if len(data) != 0 || r.dropped.Load() != 1 {
		t.Fatalf("wrote %d bytes, dropped %d", len(data), r.dropped.Load())
	}
}

func TestFullQueueDoesNotBlock(t *testing.T) {
	r := &Recorder{queue: make(chan *Exchange, 1)}
	r.Record(&Exchange{})
	r.Record(&Exchange{})
	if r.dropped.Load() != 1 {
		t.Fatalf("dropped = %d", r.dropped.Load())
	}
}
