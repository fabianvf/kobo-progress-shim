// Package activity serves the page showing recent sync activity and books
// waiting for a match. It's on the admin port, never on the Kobo's route.
package activity

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

const recentEvents = 100

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Kobo progress</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;margin:0 auto;max-width:60rem;padding:1rem;color:#222;background:#fff}
table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid #ddd;padding:.4rem;text-align:left;vertical-align:top}
input[type=text]{width:10rem}.muted{color:#666}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#181818}td,th{border-color:#444}.muted{color:#999}}
</style></head><body>
<h1>Kobo progress</h1>
{{if .DryRun}}<p class="muted">Dry run: nothing is written to Hardcover.</p>{{else}}<p class="muted">Live: changes are written to Hardcover.</p>{{end}}
<h2>Needs a match ({{len .Review}})</h2>
{{if .Review}}<table><tr><th>Book</th><th>Why</th><th>Suggested</th><th>Hardcover book id</th></tr>
{{range .Review}}<tr><td>{{.Title}}{{if .ISBN}}<br><span class="muted">{{.ISBN}}</span>{{end}}</td><td>{{.Reason}}</td><td>{{.Suggestion}}</td>
<td><form method="post" action="confirm"><input type="hidden" name="id" value="{{.EntitlementID}}"><input type="text" name="hardcover_id" inputmode="numeric" required aria-label="Hardcover book id"> <button>Save</button></form></td></tr>
{{end}}</table>{{else}}<p>Nothing waiting.</p>{{end}}
<h2>Recent activity</h2>
{{if .Events}}<table><tr><th>When (UTC)</th><th>Book</th><th>What</th></tr>
{{range .Events}}<tr><td>{{.Time.Format "2006-01-02 15:04"}}</td><td>{{if .Title}}{{.Title}}{{else}}{{.EntitlementID}}{{end}}</td><td>{{.Detail}}</td></tr>
{{end}}</table>{{else}}<p>Nothing yet.</p>{{end}}
</body></html>`))

// Register adds the page to the admin mux.
func Register(mux *http.ServeMux, books *store.Store, dryRun bool) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := load(r.Context(), books)
		if err != nil {
			log.Printf("activity: %v", err)
			http.Error(w, "couldn't read the store", http.StatusInternalServerError)
			return
		}
		data.DryRun = dryRun
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, data); err != nil {
			log.Printf("activity: %v", err)
		}
	})
	// A saved match drives public writes, and a page open in any LAN browser could
	// otherwise post here.
	sameOrigin := http.NewCrossOriginProtection()
	mux.Handle("POST /confirm", sameOrigin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(strings.TrimSpace(r.FormValue("hardcover_id")))
		if err != nil || id <= 0 {
			http.Error(w, "the Hardcover book id must be a number", http.StatusBadRequest)
			return
		}
		if _, err := books.Confirm(r.Context(), r.FormValue("id"), id); err != nil {
			log.Printf("activity: %v", err)
			http.Error(w, "couldn't save that match", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "./", http.StatusSeeOther)
	})))
}

type pageData struct {
	DryRun bool
	Review []store.Book
	Events []store.Event
}

func load(ctx context.Context, books *store.Store) (pageData, error) {
	review, err := books.NeedingReview(ctx)
	if err != nil {
		return pageData{}, err
	}
	events, err := books.RecentEvents(ctx, recentEvents)
	if err != nil {
		return pageData{}, err
	}
	return pageData{Review: review, Events: events}, nil
}
