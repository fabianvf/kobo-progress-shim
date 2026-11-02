// kobo-capture is the Phase 0 pass-through proxy. It forwards all traffic to
// CWA and writes a redacted JSONL record of each exchange. Capture is best
// effort: nothing it does may change or delay what the Kobo sees.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/dryrun"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
)

func main() {
	upstream, err := url.Parse(env("UPSTREAM_URL", "http://localhost:8083"))
	if err != nil || upstream.Host == "" {
		log.Fatalf("invalid UPSTREAM_URL")
	}

	recorder := capture.NewRecorder(env("CAPTURE_FILE", "/data/capture.jsonl"))
	defer recorder.Close()

	// Optional: without a token the worker logs store lookups but doesn't match.
	var lookup match.Lookup
	if token := os.Getenv("HARDCOVER_API_KEY"); token != "" {
		lookup = hardcover.New(token)
	}
	worker := dryrun.New(kobostore.New(), lookup)
	recorder.OnProgress = worker.Submit

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		log.Print(http.ListenAndServe(env("ADMIN_ADDR", ":8081"), mux))
	}()

	server := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           capture.NewProxy(upstream, recorder),
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    8 << 20, // sync tokens make Kobo headers unusually large
	}
	log.Fatal(server.ListenAndServe())
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
