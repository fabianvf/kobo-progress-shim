// Package setup is the page a new reader uses to join: they sign in through
// Cloudflare Access, paste their Hardcover API key, and get the one line to put
// on their Kobo. The shim makes their CWA account itself, so CWA is never theirs
// to deal with. It runs on its own port, never the Kobo's, at a path of the
// library's address.
package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
)

// Identity says who's asking. Access is the real one.
type Identity interface {
	Email(r *http.Request) (string, error)
}

// Hardcover checks a key and says whose it is.
type Hardcover func(ctx context.Context, token string) (string, error)

// CWA makes a user and returns their Kobo token.
type CWA interface {
	CreateUser(ctx context.Context, name, email, password string) (string, error)
}

// Seen reports when a user's Kobo last produced anything.
type Seen func(ctx context.Context, user string) (time.Time, error)

type Page struct {
	identity  Identity
	registry  *accounts.Registry
	hardcover Hardcover
	cwa       CWA
	seen      Seen
	publicURL string // what the Kobo's api_endpoint starts with
}

func New(identity Identity, registry *accounts.Registry, hc Hardcover, cwa CWA, seen Seen, publicURL string) *Page {
	return &Page{identity: identity, registry: registry, hardcover: hc, cwa: cwa, seen: seen, publicURL: strings.TrimSuffix(publicURL, "/")}
}

var page = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Library setup</title>
<style>
body{font:16px/1.55 system-ui,sans-serif;margin:0 auto;max-width:42rem;padding:1rem;color:#222;background:#fff}
input[type=password]{width:100%;box-sizing:border-box;padding:.5rem;font:inherit}button{padding:.5rem 1rem;font:inherit}
code,pre{background:#f3f3f3;padding:.1rem .3rem;border-radius:3px;overflow-wrap:anywhere;white-space:pre-wrap}
.muted{color:#666}.error{color:#a00}.ok{color:#060}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#181818}code,pre{background:#2a2a2a}.muted{color:#999}}
</style></head><body>
<h1>Library setup</h1>
<p class="muted">Signed in as {{.Email}}.</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
{{if .Endpoint}}
<p class="ok">Connected to Hardcover as {{.HardcoverUser}}.</p>
<h2>Your Kobo</h2>
<ol>
<li>Connect your Kobo to a computer with USB and open the <code>.kobo/Kobo</code> folder on it.</li>
<li>Open <code>Kobo eReader.conf</code> in a text editor and find the line starting <code>api_endpoint=</code> under <code>[OneStoreServices]</code>. Add it there if it's missing.</li>
<li>Replace that line with this one, then save, eject and sync:</li>
</ol>
<pre>api_endpoint={{.Endpoint}}</pre>
<p>{{if .Seen}}Your Kobo last synced here {{.Seen}}.{{else}}Your Kobo hasn't synced here yet.{{end}}</p>
<h2>How it works</h2>
<p>Your Kobo's reading goes to Hardcover. Everything on your Hardcover shelves and lists shows on your Kobo, in collections named after them,
and books on Want to Read that the library doesn't have are fetched, usually within a day. Books finished on Hardcover show as finished.</p>
<h2>Change your Hardcover key</h2>
{{else}}
<p>Paste your Hardcover API key. You'll find it at <a href="https://hardcover.app/account/api">hardcover.app/account/api</a>. It's kept encrypted and only used for your account.</p>
{{end}}
<form method="post"><input type="password" name="hardcover_key" required autocomplete="off" aria-label="Hardcover API key"><p><button>Save</button></p></form>
</body></html>`))

type pageData struct {
	Email, Error, Endpoint, HardcoverUser, Seen string
}

// Register serves the page at prefix, such as "/setup/". The tunnel sends that
// path of the library's address here, so it sits behind the same Access login.
func (p *Page) Register(mux *http.ServeMux, prefix string) {
	prefix = "/" + strings.Trim(prefix, "/") + "/"
	if prefix == "//" {
		prefix = "/"
	}
	same := http.NewCrossOriginProtection()
	mux.HandleFunc("GET "+prefix+"{$}", p.show)
	mux.Handle("POST "+prefix+"{$}", same.Handler(http.HandlerFunc(p.save)))
	if prefix != "/" {
		mux.Handle("GET "+strings.TrimSuffix(prefix, "/"), http.RedirectHandler(prefix, http.StatusMovedPermanently))
	}
}

func (p *Page) show(w http.ResponseWriter, r *http.Request) {
	email, err := p.identity.Email(r)
	if err != nil {
		http.Error(w, "sign in through the library's address", http.StatusUnauthorized)
		return
	}
	p.render(w, r, email, "")
}

func (p *Page) render(w http.ResponseWriter, r *http.Request, email, problem string) {
	data := pageData{Email: email, Error: problem}
	if a, ok := p.registry.ByEmail(email); ok {
		data.Endpoint = p.publicURL + "/kobo/" + a.KoboToken
		data.HardcoverUser = a.Name
		if when, err := p.seen(r.Context(), a.Key()); err == nil && !when.IsZero() {
			data.Seen = when.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // the page shows a token
	if err := page.Execute(w, data); err != nil {
		log.Printf("setup: %v", err)
	}
}

func (p *Page) save(w http.ResponseWriter, r *http.Request) {
	email, err := p.identity.Email(r)
	if err != nil {
		http.Error(w, "sign in through the library's address", http.StatusUnauthorized)
		return
	}
	key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.FormValue("hardcover_key")), "Bearer "))
	user, err := p.hardcover(r.Context(), key)
	if err != nil {
		p.render(w, r, email, "Hardcover didn't accept that key. Copy it again from hardcover.app/account/api.")
		return
	}
	a, ok := p.registry.ByEmail(email)
	if !ok {
		password, err := newPassword()
		if err == nil {
			a = accounts.Account{Email: email, CWAUser: cwaName(email), CWAPassword: password}
			a.KoboToken, err = p.cwa.CreateUser(r.Context(), a.CWAUser, email, password)
		}
		if err != nil {
			log.Printf("setup %s: %v", accounts.Key(email), err)
			p.render(w, r, email, "Setting up your library account failed. Ask Fabian to look at the shim's log.")
			return
		}
	}
	a.Name, a.HardcoverToken = user, key
	if err := p.registry.Save(r.Context(), a); err != nil {
		log.Printf("setup %s: %v", accounts.Key(email), err)
		p.render(w, r, email, "Saving your account failed. Ask Fabian to look at the shim's log.")
		return
	}
	log.Printf("setup: account %s saved for Hardcover user %s", a.Key(), user)
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// cwaName is the CWA username for an email, which nobody sees.
func cwaName(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	return "kobo-" + unsafeName.ReplaceAllString(local, "-")
}

// newPassword is random, plus one of each character class in case CWA's
// password policy is on.
func newPassword() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "aA1!", nil
}
