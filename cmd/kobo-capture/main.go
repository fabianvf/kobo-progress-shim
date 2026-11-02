// kobo-capture is the pass-through proxy. It forwards all traffic to CWA, writes
// a redacted JSONL record of each exchange, and works out in dry-run what it
// would send to Hardcover. All of that is best effort: nothing it does may change
// or delay what the Kobo sees.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/activity"
	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/dryrun"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

func main() {
	upstream, err := url.Parse(env("UPSTREAM_URL", "http://localhost:8083"))
	if err != nil || upstream.Host == "" {
		log.Fatalf("invalid UPSTREAM_URL")
	}

	recorder := capture.NewRecorder(env("CAPTURE_FILE", "/data/capture.jsonl"))
	defer recorder.Close()

	admin := http.NewServeMux()
	admin.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// A store that won't open costs the matching, never the Kobo's path.
	books, err := store.Open(env("STORE_FILE", "/data/shim.db"))
	if err != nil {
		log.Printf("matching disabled: %v", err)
	} else {
		defer books.Close()
		// Optional: without a token the worker saves lookups but doesn't match.
		var lookup match.Lookup
		var writer dryrun.Writer
		if token := os.Getenv("HARDCOVER_API_KEY"); token != "" {
			client := hardcover.New(token)
			lookup, writer = client, client
		}
		// Anything but an explicit false stays dry, because a live write publishes.
		dryRun := os.Getenv("DRY_RUN") != "false"
		log.Printf("dry run: %v", dryRun)
		worker := dryrun.New(kobostore.New(), lookup, writer, books, dryRun)
		defer worker.Close()
		recorder.Observer = worker
		activity.Register(admin, books, dryRun)
	}

	go func() {
		log.Print(http.ListenAndServe(env("ADMIN_ADDR", ":8081"), admin))
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
