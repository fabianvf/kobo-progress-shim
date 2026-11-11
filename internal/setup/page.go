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
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
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

// NextUp is the reader's switch for keeping the next book in each series they're
// reading on Want to Read and in a Next Up collection.
type NextUp interface {
	NextUp(ctx context.Context, user string) (bool, error)
	SetNextUp(ctx context.Context, user string, on bool) error
}

type Page struct {
	identity  Identity
	registry  *accounts.Registry
	hardcover Hardcover
	cwa       CWA
	seen      Seen
	nextUp    NextUp
	publicURL string // what the Kobo's api_endpoint starts with
}

func New(identity Identity, registry *accounts.Registry, hc Hardcover, cwa CWA, seen Seen, nextUp NextUp, publicURL string) *Page {
	return &Page{identity: identity, registry: registry, hardcover: hc, cwa: cwa, seen: seen, nextUp: nextUp, publicURL: strings.TrimSuffix(publicURL, "/")}
}

// newKeyURL opens Hardcover's New API Key form with only the scopes the shim
// uses ticked: the profile name, the library and lists to read, the catalog to
// match against, and the library to write progress to.
const newKeyURL = "https://hardcover.app/account/api/keys/new?scope=read:me:content+read:library+read:lists+read:catalog+write:library"

var page = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Library setup</title>
<style>
:root{--bg:#f6f3ee;--card:#fff;--ink:#23201c;--muted:#6b645b;--line:#e4ddd2;--accent:#2f6f5e;--accent-ink:#fff;--code:#f1ece4;--ok:#2f6f5e;--err:#a33a2a}
@media (prefers-color-scheme:dark){:root{--bg:#17161a;--card:#211f24;--ink:#ece7df;--muted:#a59e94;--line:#34313a;--accent:#6fbf9f;--accent-ink:#10251e;--code:#2b2830;--ok:#8fd4b5;--err:#f08d7d}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:17px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:40rem;margin:0 auto;padding:2rem 1rem 4rem}
h1{font-size:1.9rem;line-height:1.2;margin:0 0 .3rem}
h2{font-size:1.2rem;margin:0 0 .8rem;display:flex;gap:.6rem;align-items:center}
.lede{color:var(--muted);margin:0 0 1.5rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:1.3rem 1.4rem;margin:1rem 0}
.num{flex:none;width:1.8rem;height:1.8rem;border-radius:50%;background:var(--accent);color:var(--accent-ink);display:grid;place-items:center;font-size:.95rem;font-weight:700}
.done .num{background:var(--ok)}
ol,ul{padding-left:1.3rem;margin:.4rem 0}li{margin:.45rem 0}
a{color:var(--accent)}
.button{display:inline-block;background:var(--accent);color:var(--accent-ink);text-decoration:none;font-weight:600;border:0;border-radius:9px;padding:.65rem 1.1rem;font:inherit;font-weight:600;cursor:pointer}
input[type=password]{width:100%;padding:.7rem .8rem;font:inherit;border:1px solid var(--line);border-radius:9px;background:var(--bg);color:var(--ink)}
code,kbd{background:var(--code);padding:.08rem .35rem;border-radius:5px;font-size:.92em;overflow-wrap:anywhere}
.line{display:flex;gap:.5rem;align-items:stretch;margin:.6rem 0}
.line pre{flex:1;margin:0;background:var(--code);border-radius:9px;padding:.7rem .8rem;white-space:pre-wrap;overflow-wrap:anywhere;font-size:.9rem}
.muted{color:var(--muted)}.ok{color:var(--ok);font-weight:600}.error{color:var(--err);font-weight:600}
details{border-top:1px solid var(--line);padding:.6rem 0}details:first-of-type{border-top:0}summary{cursor:pointer;font-weight:600}
.status{padding:.7rem .9rem;border-radius:9px;background:var(--code)}
</style></head><body><main>
<h1>Welcome to the library</h1>
<p class="lede">Signed in as {{.Email}}. This connects your Kobo to the family library and to
<a href="https://hardcover.app">Hardcover</a>, a free site for tracking what you read. After this you only need two things:
Hardcover to pick books, and your Kobo to read them.</p>
{{if .Error}}<p class="card error">{{.Error}}</p>{{end}}

{{if .Endpoint}}
{{if .KeyRejected}}<section class="card"><h2><span class="num">!</span>Your Hardcover key stopped working</h2>
<p class="error">Hardcover isn't accepting your key any more, so your reading isn't syncing. It has probably expired.</p>
{{if .Configured}}<p>Your account is configured on the server, so ask Fabian to replace it.</p>
{{else}}<p><a class="button" href="` + newKeyURL + `" target="_blank" rel="noopener">Make a new Hardcover key</a></p>
<p>Choose <b>Never</b> for <b>Expiration</b> this time, then paste the new key at the bottom of this page and press Save.</p>{{end}}
</section>
{{else}}<section class="card done"><h2><span class="num">✓</span>Hardcover is connected</h2>
<p class="ok">Connected as {{.HardcoverUser}}.</p></section>{{end}}

<section class="card"><h2><span class="num">2</span>Point your Kobo at the library</h2>
<p>A one-time change to one line in a settings file on your Kobo. It takes about five minutes, and you can undo it any time.</p>
<ol>
<li>Plug your Kobo into a computer with its USB cable, and tap <b>Connect</b> on the Kobo when it asks.</li>
<li>Open the Kobo on your computer like a USB stick. It's called <b>KOBOeReader</b>. The folder you need is hidden, so show hidden files first:
<ul><li><b>Mac:</b> open KOBOeReader in Finder and press <kbd>⌘ Cmd</kbd> <kbd>⇧ Shift</kbd> <kbd>.</kbd></li>
<li><b>Windows 11:</b> in File Explorer choose <b>View</b> → <b>Show</b> → <b>Hidden items</b>. On Windows 10 it's the <b>View</b> tab → <b>Hidden items</b>.</li></ul></li>
<li>Open the <code>.kobo</code> folder, then the <code>Kobo</code> folder inside it.</li>
<li>Open <code>Kobo eReader.conf</code> in a plain text editor: right-click it and choose <b>Open With → TextEdit</b> on a Mac, or <b>Open with → Notepad</b> on Windows.</li>
<li>Find the line starting <code>api_endpoint=</code>. It's under the heading <code>[OneStoreServices]</code>. Replace the whole line with yours:
<div class="line"><pre id="endpoint">api_endpoint={{.Endpoint}}</pre><button class="button" type="button" onclick="navigator.clipboard.writeText(document.getElementById('endpoint').textContent).then(()=>{this.textContent='Copied'})">Copy</button></div>
If there's no <code>api_endpoint=</code> line, add yours on a new line just below <code>[OneStoreServices]</code>. Keep it private: it's how the library knows your Kobo is yours.</li>
<li>Save the file, eject the Kobo from your computer, and unplug it.</li>
<li>Sync the Kobo: tap the sync icon (two circling arrows) at the top of the home screen.</li>
</ol>
<p class="status">{{if .Seen}}✓ Your Kobo has synced with the library, most recently {{.Seen}}.{{else}}Your Kobo hasn't synced with the library yet. Reload this page after step 7 to check.{{end}}</p>
</section>

<section class="card"><h2>Next in your series</h2>
<form method="post" action="next-up">
<p><label><input type="checkbox" name="on" value="1"{{if .NextUp}} checked{{end}} onchange="this.form.submit()">
<b>Keep track of the next book in each series I'm reading</b></label></p>
<p class="muted">For every series where you've read or are reading a book, the library finds the next one that's out, and within the hour:</p>
<ul class="muted">
<li>marks it <b>Want to Read</b> on Hardcover if it isn't on one of your shelves yet, so it's fetched like any other book you want. Hardcover may show that on your profile's activity.</li>
<li>keeps a <b>Next Up</b> collection on your Kobo with just those books.</li>
</ul>
<p class="muted">Each book is only added once, so if you take one off Want to Read it stays off. Series you paused or didn't finish are skipped.
Turning this off removes the Next Up collection, and leaves your Want to Read as it is.</p>
<noscript><p><button class="button">Save</button></p></noscript>
</form></section>

<section class="card"><h2>What happens next</h2>
<ul>
<li>Your Kobo gets a collection called <b>Hardcover</b> with every book on your Hardcover shelves and lists, plus one for each shelf and list,
like <b>Want to Read</b> and <b>Read</b>. Find them under <b>My Books</b> → <b>Collections</b>.</li>
<li>To get a new book, <a href="https://hardcover.app/search">find it on Hardcover</a> and mark it <b>Want to Read</b>. If the library doesn't have it yet,
it's fetched, usually within a day, and shows up after your next sync.</li>
<li>Your progress goes to Hardcover whenever the Kobo syncs, and finishing a book marks it read. Books you mark read on Hardcover show as finished on your Kobo.</li>
<li>Books you've bought from Kobo, and Libby loans, keep working as before.</li>
</ul></section>

{{if .CWAUser}}<section class="card"><h2>Browse the library on the web (optional)</h2>
<p>You never need this, but you can see the whole library and your collections at <a href="{{.LibraryURL}}">{{.LibraryURL}}</a>.
After the family sign-in, log in with:</p>
<div class="line"><pre id="cwa-user">{{.CWAUser}}</pre><button class="button" type="button" onclick="navigator.clipboard.writeText(document.getElementById('cwa-user').textContent).then(()=>{this.textContent='Copied'})">Copy</button></div>
<div class="line"><pre id="cwa-pass">{{.CWAPassword}}</pre><button class="button" type="button" onclick="navigator.clipboard.writeText(document.getElementById('cwa-pass').textContent).then(()=>{this.textContent='Copied'})">Copy</button></div>
<p class="muted">The library uses this password to keep your Kobo's collections in sync, so it can't be changed there.</p>
</section>{{end}}

<section class="card"><h2>If something goes wrong</h2>
<details><summary>The Kobo says it can't sync</summary><p>Plug it back in and check the <code>api_endpoint=</code> line matches yours above exactly, with nothing else on the line.</p></details>
<details><summary>It stopped working after a while</summary><p>If you didn't choose <b>Never</b> when making your Hardcover key, it has expired. Make a new one with the link below and paste it here.
If it stopped after a Kobo update or reset, check the <code>api_endpoint=</code> line is still there.</p></details>
<details><summary>A book isn't showing up</summary><p>Make sure it's on one of your Hardcover shelves or lists, give it a day if the library had to fetch it, then sync. Still missing? Ask Fabian.</p></details>
<details><summary>Undo everything</summary><p>Change the line back to <code>api_endpoint=https://storeapi.kobo.com</code> and sync. Your Kobo goes back to syncing with Kobo only,
and books from the library may disappear from it.</p></details>
</section>

<section class="card"><h2>{{if .Configured}}Your Hardcover key{{else}}Replace your Hardcover key{{end}}</h2>
<p class="muted">Only needed if your key expired or you made a new one. <a href="` + newKeyURL + `">Make a new key</a>, then paste it here.</p>
{{else}}
<section class="card"><h2><span class="num">1</span>Connect Hardcover</h2>
<ol>
<li>No Hardcover account yet? <a href="https://hardcover.app">Join Hardcover</a> first. It's free.</li>
<li>Put a few books on your <b>Want to Read</b> shelf: <a href="https://hardcover.app/search">search for a book</a>, open it, and choose <b>Want to Read</b>.
You can add more any time, and each one will come to your Kobo.</li>
<li>Make a key for the library: <a class="button" href="` + newKeyURL + `" target="_blank" rel="noopener">Make a Hardcover key</a><br>
Hardcover's form opens with the right permissions already ticked. Name it <b>Library</b> and change <b>Expiration</b> to <b>Never</b>,
since the library stops syncing your reading when the key expires. Press <b>Create Key</b> and copy the key it shows you. It's only shown once.</li>
<li>Paste it here and press Save. It's stored encrypted and only used for your own account.</li>
</ol>
{{end}}
{{if .Configured}}<p class="muted">Your account is configured on the server, so its Hardcover key is changed there, not here.</p>
{{else}}<form method="post"><input type="password" name="hardcover_key" required autocomplete="off" aria-label="Hardcover API key" placeholder="Paste your Hardcover key">
<p><button class="button">Save</button></p></form>{{end}}
</section>
</main></body></html>`))

type pageData struct {
	Configured                                  bool // set up by Fabian, not through this page
	KeyRejected                                 bool // Hardcover refuses the saved key
	NextUp                                      bool
	Email, Error, Endpoint, HardcoverUser, Seen string
	LibraryURL, CWAUser, CWAPassword            string
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
	mux.Handle("POST "+prefix+"next-up", same.Handler(http.HandlerFunc(p.setNextUp)))
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
	if a, configured, ok := p.registry.ByEmail(email); ok {
		data.Configured = configured
		data.Endpoint = p.publicURL + "/kobo/" + a.KoboToken
		data.HardcoverUser = a.Name
		data.LibraryURL, data.CWAUser, data.CWAPassword = p.publicURL+"/", a.CWAUser, a.CWAPassword
		if on, err := p.nextUp.NextUp(r.Context(), a.Key()); err == nil {
			data.NextUp = on
		}
		if when, err := p.seen(r.Context(), a.Key()); err == nil && !when.IsZero() {
			data.Seen = when.UTC().Format("2006-01-02 15:04 UTC")
		}
		// Only a refusal counts. Hardcover being down isn't the reader's to fix.
		if _, err := p.hardcover(r.Context(), a.HardcoverToken); errors.Is(err, hardcover.ErrRejected) {
			data.KeyRejected = true
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
	a, configured, ok := p.registry.ByEmail(email)
	if configured {
		p.render(w, r, email, "Your account is configured on the server, so its Hardcover key is changed there, not here.")
		return
	}
	if !ok {
		password, err := newPassword()
		if err == nil {
			a = accounts.Account{Email: email, CWAPassword: password}
			// Two emails can share the part before the @, so a taken name gets a number.
			for n := 1; n <= 5; n++ {
				a.CWAUser = cwaName(user, email)
				if n > 1 {
					a.CWAUser = fmt.Sprintf("%s-%d", a.CWAUser, n)
				}
				a.KoboToken, err = p.cwa.CreateUser(r.Context(), a.CWAUser, email, password)
				if err == nil || !strings.Contains(err.Error(), "username is already taken") {
					break
				}
			}
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

// setNextUp works for a configured account too, since it's kept in the store.
func (p *Page) setNextUp(w http.ResponseWriter, r *http.Request) {
	email, err := p.identity.Email(r)
	if err != nil {
		http.Error(w, "sign in through the library's address", http.StatusUnauthorized)
		return
	}
	a, _, ok := p.registry.ByEmail(email)
	if !ok {
		p.render(w, r, email, "Connect Hardcover first.")
		return
	}
	on := r.FormValue("on") != ""
	if err := p.nextUp.SetNextUp(r.Context(), a.Key(), on); err != nil {
		log.Printf("setup %s: %v", accounts.Key(email), err)
		p.render(w, r, email, "Saving that failed. Ask Fabian to look at the shim's log.")
		return
	}
	log.Printf("setup: account %s next up %v", a.Key(), on)
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// cwaName is the reader's Hardcover username, which is unique on Hardcover and
// what they know themselves by. The email's first part stands in if it's empty.
func cwaName(hardcoverUser, email string) string {
	name := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(hardcoverUser), "-"), "-")
	if name == "" {
		local, _, _ := strings.Cut(strings.ToLower(email), "@")
		name = "reader-" + unsafeName.ReplaceAllString(local, "-")
	}
	return name
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
