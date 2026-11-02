// Package dryrun identifies books from live progress and logs what it would do.
// It writes nothing to Hardcover and nothing it does can reach the Kobo.
package dryrun

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
)

const queueSize = 64

// Store is the Kobo metadata lookup.
type Store interface {
	Metadata(ctx context.Context, id, authorization, userAgent string) (kobostore.Metadata, error)
}

// Entry is one log line. It has no auth field, so it is safe to marshal whole.
type Entry struct {
	EntitlementID string   `json:"entitlement_id"`
	Percent       int      `json:"percent"`
	Status        string   `json:"status"`
	LookupResult  string   `json:"lookup_result"`
	Title         string   `json:"title,omitempty"`
	Authors       []string `json:"authors,omitempty"`
	ISBN          string   `json:"isbn,omitempty"`
	ISBNFrom      string   `json:"isbn_from,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	BookID        int      `json:"hardcover_book_id,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	Suggestion    string   `json:"suggestion,omitempty"`
}

type Worker struct {
	store  Store
	lookup match.Lookup // nil when there's no Hardcover token
	queue  chan capture.Progress
	known  map[string]Entry
	done   sync.WaitGroup
}

func New(store Store, lookup match.Lookup) *Worker {
	w := &Worker{store: store, lookup: lookup, queue: make(chan capture.Progress, queueSize), known: map[string]Entry{}}
	w.done.Add(1)
	go w.run()
	return w
}

// Submit never blocks. A dropped event is harmless because the next page turn
// and sync report the same book again.
func (w *Worker) Submit(p capture.Progress) {
	select {
	case w.queue <- p:
	default:
		log.Print("dryrun queue full, event dropped")
	}
}

func (w *Worker) Close() {
	close(w.queue)
	w.done.Wait()
}

func (w *Worker) run() {
	defer w.done.Done()
	for p := range w.queue {
		w.handle(p)
	}
}

func (w *Worker) handle(p capture.Progress) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("dryrun failed: %T", recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	entry := w.identify(ctx, p)
	entry.Percent, entry.Status = p.Percent, p.Status
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("dryrun marshal failed: %T", err)
		return
	}
	log.Printf("dryrun %s", line)
}

// identify remembers an answer only once it's complete, so a failed lookup is
// tried again on the book's next progress update.
func (w *Worker) identify(ctx context.Context, p capture.Progress) Entry {
	if known, ok := w.known[p.EntitlementID]; ok {
		return known
	}
	entry := Entry{EntitlementID: p.EntitlementID}
	complete := true

	signals := match.Signals{}
	meta, err := w.store.Metadata(ctx, p.EntitlementID, p.Authorization, p.UserAgent)
	switch {
	case err == nil:
		entry.LookupResult = "found"
		signals.Title, signals.Authors, signals.ISBN = meta.Title, meta.Contributors, meta.ISBN
		if meta.ISBN != "" {
			entry.ISBNFrom = "store"
		}
	case errors.Is(err, kobostore.ErrNotFound):
		entry.LookupResult = "not_found"
	default:
		entry.LookupResult = "error: " + err.Error()
		complete = false
	}
	if signals.ISBN == "" {
		if signals.ISBN = match.ISBNFromPath(p.Source); signals.ISBN != "" {
			entry.ISBNFrom = "bookmark"
		}
	}
	entry.Title, entry.Authors, entry.ISBN = signals.Title, signals.Authors, signals.ISBN

	if w.lookup != nil {
		decision, err := match.Decide(ctx, w.lookup, signals)
		if err != nil {
			entry.Reason = "hardcover error: " + err.Error()
			complete = false
		} else {
			entry.Outcome, entry.BookID, entry.Reason = string(decision.Outcome), decision.BookID, decision.Reason
			if len(decision.Candidates) > 0 {
				entry.Suggestion = decision.Candidates[0].Title
			}
		}
	}
	if complete {
		w.known[p.EntitlementID] = entry
	}
	return entry
}
