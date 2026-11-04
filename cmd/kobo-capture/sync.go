package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
	"github.com/fabianvf/kobo-progress-shim/internal/cwasync"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/setup"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
	"github.com/fabianvf/kobo-progress-shim/internal/wanted"
)

// loadAccounts reads the configured accounts and any made through setup, which
// needs ACCOUNTS_KEY to open. A bad key leaves only the configured ones.
func loadAccounts(books *store.Store) *accounts.Registry {
	ctx := context.Background()
	fixed, err := accounts.Load()
	if err != nil {
		log.Printf("no configured accounts: %v", err)
	}
	registry, err := accounts.NewRegistry(ctx, fixed, books, os.Getenv("ACCOUNTS_KEY"))
	if err != nil {
		log.Printf("setup accounts unavailable: %v", err)
		registry, _ = accounts.NewRegistry(ctx, fixed, books, "")
	}
	if all := registry.All(); len(all) == 1 && all[0].Key() != accounts.Any {
		if err := books.AdoptUnowned(ctx, all[0].Key()); err != nil {
			log.Printf("adopting saved books: %v", err)
		}
	}
	withKobo := 0
	for _, a := range registry.All() {
		if a.KoboToken != "" {
			withKobo++
		}
	}
	log.Printf("accounts: %d, %d with a Kobo token, setup %v", len(registry.All()), withKobo, registry.SetupEnabled())
	return registry
}

// clients keeps one Hardcover client per token, so each keeps its own spacing.
type clients struct {
	mu sync.Mutex
	by map[string]*hardcover.Client
}

func newClients() *clients { return &clients{by: map[string]*hardcover.Client{}} }

func (c *clients) get(token string) *hardcover.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.by[token]; ok {
		return client
	}
	client := hardcover.New(token)
	c.by[token] = client
	return client
}

// startSync runs the CWA sync, Want to Read downloads and the setup page, each
// only when what it needs is configured. None of it is on the Kobo's path.
func startSync(upstream *url.URL, books *store.Store, registry *accounts.Registry, hc *clients, dryRun bool) {
	robot, password := os.Getenv("CWA_USER"), os.Getenv("CWA_PASSWORD")
	if robot == "" || password == "" {
		log.Print("cwa sync off: no CWA_USER and CWA_PASSWORD")
		return
	}
	base, err := url.Parse(strings.TrimSuffix(env("CWA_URL", upstream.String()), "/") + "/")
	interval, durErr := time.ParseDuration(env("CWA_SYNC_INTERVAL", "1h"))
	if err != nil || durErr != nil || interval < time.Minute {
		log.Print("cwa sync off: bad CWA_URL or CWA_SYNC_INTERVAL")
		return
	}
	var downloads cwasync.Downloader
	if llURL, key := os.Getenv("LL_URL"), os.Getenv("LL_API_KEY"); llURL != "" && key != "" {
		if llBase, err := url.Parse(strings.TrimSuffix(llURL, "/") + "/"); err == nil {
			viaHardcover := os.Getenv("LL_SOURCE") == "HardCover"
			downloads = wanted.New(wanted.NewOpenLibrary(), wanted.NewLazyLibrarian(llBase, key), books, viaHardcover, dryRun)
			log.Printf("want to read downloads on, through %s", map[bool]string{true: "LazyLibrarian's Hardcover source", false: "OpenLibrary"}[viaHardcover])
		}
	}
	synced := func() []cwasync.Account {
		var out []cwasync.Account
		for _, a := range registry.All() {
			if a.KoboToken != "" {
				out = append(out, cwasync.Account{Key: a.Key(), KoboToken: a.KoboToken, Hardcover: hc.get(a.HardcoverToken), CWAUser: a.CWAUser, CWAPassword: a.CWAPassword})
			}
		}
		return out
	}
	log.Printf("cwa sync every %s", interval)
	team, aud, public := os.Getenv("ACCESS_TEAM"), os.Getenv("ACCESS_AUD"), os.Getenv("PUBLIC_URL")
	setupURL := ""
	if public != "" {
		setupURL = strings.TrimSuffix(public, "/") + "/" + strings.Trim(env("SETUP_PATH", "/setup/"), "/")
	}
	go cwasync.New(cwasync.NewCWA(base, robot, password), shelves{base}, downloads, books, synced, dryRun).WithSetup(setupURL).Run(context.Background(), 2*time.Minute, interval)

	if !registry.SetupEnabled() || team == "" || aud == "" || public == "" {
		log.Print("setup page off: needs ACCOUNTS_KEY, ACCESS_TEAM, ACCESS_AUD and PUBLIC_URL")
		return
	}
	username := func(ctx context.Context, token string) (string, error) { return hardcover.New(token).Username(ctx) }
	mux := http.NewServeMux()
	setup.New(setup.NewAccess(team, aud), registry, username, robotAdmin{base, robot, password}, books.LastEvent, public).Register(mux, env("SETUP_PATH", "/setup/"))
	go func() { log.Print(http.ListenAndServe(env("SETUP_ADDR", ":8082"), mux)) }()
	log.Print("setup page on")
}

// shelves logs in as a user to make a shelf that syncs, which only they can do.
type shelves struct{ base *url.URL }

func (s shelves) CreateSyncedShelf(ctx context.Context, user, password, name string) error {
	session, err := cwasync.Login(ctx, s.base, user, password)
	if err != nil {
		return err
	}
	return session.CreateSyncedShelf(ctx, name)
}

func (s shelves) MarkShelfSynced(ctx context.Context, user, password, name string) error {
	session, err := cwasync.Login(ctx, s.base, user, password)
	if err != nil {
		return err
	}
	return session.MarkShelfSynced(ctx, name)
}

// robotAdmin makes CWA users as the robot account.
type robotAdmin struct {
	base           *url.URL
	user, password string
}

func (r robotAdmin) CreateUser(ctx context.Context, name, email, password string) (string, error) {
	session, err := cwasync.Login(ctx, r.base, r.user, r.password)
	if err != nil {
		return "", err
	}
	_, token, err := session.CreateUser(ctx, cwasync.NewUser{Name: name, Email: email, Password: password})
	return token, err
}
