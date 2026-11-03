// kobo-capture is the pass-through proxy. It forwards all traffic to CWA, writes
// a redacted JSONL record of each exchange, sends what the Kobo reports to
// Hardcover, and keeps CWA's read status following Hardcover. All of that is best
// effort: nothing it does may change or delay what the Kobo sends or receives.
package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
	"github.com/fabianvf/kobo-progress-shim/internal/activity"
	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/cwasync"
	"github.com/fabianvf/kobo-progress-shim/internal/dryrun"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
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
		list, err := accounts.Load()
		if err != nil {
			log.Printf("no accounts: %v", err)
		}
		clients := map[string]*hardcover.Client{}
		var synced []cwasync.Account
		for _, a := range list {
			client := hardcover.New(a.HardcoverToken)
			clients[a.Key()] = client
			if a.KoboToken != "" {
				synced = append(synced, cwasync.Account{Key: a.Key(), KoboToken: a.KoboToken, Hardcover: client})
			}
		}
		if len(list) == 1 && list[0].Key() != accounts.Any {
			if err := books.AdoptUnowned(context.Background(), list[0].Key()); err != nil {
				log.Printf("adopting saved books: %v", err)
			}
		}
		log.Printf("accounts: %d, %d with a Kobo token", len(list), len(synced))
		byUser := func(user string) dryrun.Account {
			client, ok := clients[user]
			if !ok {
				client, ok = clients[accounts.Any]
			}
			if !ok {
				return dryrun.Account{}
			}
			return dryrun.Account{Lookup: client, Writer: client}
		}
		// Anything but an explicit false stays dry, because a live write publishes.
		dryRun := os.Getenv("DRY_RUN") != "false"
		log.Printf("dry run: %v", dryRun)
		worker := dryrun.New(kobostore.New(), byUser, books, dryRun)
		defer worker.Close()
		recorder.Observer = worker
		activity.Register(admin, books, dryRun)

		// CWA's read status follows Hardcover. It needs the robot account for the
		// library list and a Kobo token per user, and is off without them.
		robot, password := os.Getenv("CWA_USER"), os.Getenv("CWA_PASSWORD")
		if robot != "" && password != "" && len(synced) > 0 {
			base, err := url.Parse(strings.TrimSuffix(env("CWA_URL", upstream.String()), "/") + "/")
			interval, durErr := time.ParseDuration(env("CWA_SYNC_INTERVAL", "1h"))
			if err != nil || durErr != nil || interval < time.Minute {
				log.Printf("cwa sync off: bad CWA_URL or CWA_SYNC_INTERVAL")
			} else {
				log.Printf("cwa sync every %s", interval)
				go cwasync.New(cwasync.NewCWA(base, robot, password), books, synced, dryRun).Run(context.Background(), 2*time.Minute, interval)
			}
		}
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
