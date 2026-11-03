// kobo-capture is the pass-through proxy. It forwards all traffic to CWA, writes
// a redacted JSONL record of each exchange, sends what the Kobo reports to
// Hardcover, and keeps CWA's read status following Hardcover. All of that is best
// effort: nothing it does may change or delay what the Kobo sends or receives.
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
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
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
		// Optional: without an account the worker saves lookups but doesn't match.
		registry := loadAccounts(books)
		clients := newClients()
		byUser := func(user string) dryrun.Account {
			a, ok := registry.ByKey(user)
			if !ok {
				return dryrun.Account{}
			}
			client := clients.get(a.HardcoverToken)
			return dryrun.Account{Lookup: client, Writer: client}
		}
		// Anything but an explicit false stays dry, because a live write publishes.
		dryRun := os.Getenv("DRY_RUN") != "false"
		log.Printf("dry run: %v", dryRun)
		worker := dryrun.New(kobostore.New(), byUser, books, dryRun)
		defer worker.Close()
		recorder.Observer = worker
		activity.Register(admin, books, dryRun)
		startSync(upstream, books, registry, clients, dryRun)
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
