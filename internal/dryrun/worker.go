// Package dryrun identifies books from live traffic and sends their state to
// Hardcover, or only records what it would send when dry-run is on, which is the
// default. Nothing it does can reach the Kobo.
package dryrun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

const (
	queueSize     = 256
	retryInterval = 5 * time.Minute
)

// Hardcover shelves, by what the Kobo reported.
const (
	shelfReading = "reading"
	shelfRead    = "read"
)

// Store is the Kobo metadata lookup.
type Store interface {
	Metadata(ctx context.Context, id string, device http.Header) (kobostore.Metadata, error)
}

// Writer moves Hardcover to the wanted state, or with dryRun says what it would do.
type Writer interface {
	Reconcile(ctx context.Context, want hardcover.Want, dryRun bool) ([]string, error)
}

type Worker struct {
	kobo   Store
	lookup match.Lookup // nil when there's no Hardcover token
	writer Writer       // nil when there's no Hardcover token
	dryRun bool
	books  *store.Store
	queue  chan any
	done   sync.WaitGroup
}

func New(kobo Store, lookup match.Lookup, writer Writer, books *store.Store, dryRun bool) *Worker {
	w := &Worker{kobo: kobo, lookup: lookup, writer: writer, dryRun: dryRun, books: books, queue: make(chan any, queueSize)}
	w.done.Add(1)
	go w.run()
	return w
}

// These never block. A dropped event is harmless because the Kobo reports the
// book again on its next page turn and sync.
func (w *Worker) Progress(p capture.Progress)     { w.submit(p) }
func (w *Worker) Rating(r capture.Rating)         { w.submit(r) }
func (w *Worker) SyncedBook(b capture.SyncedBook) { w.submit(b) }

func (w *Worker) submit(item any) {
	select {
	case w.queue <- item:
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
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	for {
		select {
		case item, ok := <-w.queue:
			if !ok {
				return
			}
			w.handle(item)
		case <-retry.C:
			w.handle(retryPending{})
		}
	}
}

// retryPending sends books a failed write or a hand-chosen match left behind.
type retryPending struct{}

func (w *Worker) handleRetry(ctx context.Context) error {
	pending, err := w.books.PendingBooks(ctx)
	if err != nil {
		return err
	}
	for _, b := range pending {
		if err := w.write(ctx, &b); err != nil {
			return err
		}
	}
	return nil
}

// write sends a book's latest state and clears Pending only when that worked, so
// a failure is retried and a restart loses nothing.
func (w *Worker) write(ctx context.Context, b *store.Book) error {
	if w.writer == nil || !b.Resolved() || b.Shelf == "" {
		return nil
	}
	want := hardcover.Want{BookID: b.HardcoverID, ISBN: b.ISBN, Read: b.Shelf == shelfRead, Percent: b.Percent, Date: b.Date}
	if b.Rating != b.RatingWritten {
		want.Rating = b.Rating
	}
	steps, err := w.writer.Reconcile(ctx, want, w.dryRun)
	detail := strings.Join(steps, "; ")
	if detail == "" {
		detail = "Hardcover already up to date"
	}
	if w.dryRun {
		detail = "would " + detail
	}
	if err != nil {
		b.Pending = true
		if putErr := w.books.Put(ctx, *b); putErr != nil {
			return putErr
		}
		return w.record(ctx, *b, fmt.Sprintf("write failed, will retry: %v", err))
	}
	b.Pending = false
	if !w.dryRun {
		b.RatingWritten = b.Rating
	}
	if err := w.books.Put(ctx, *b); err != nil {
		return err
	}
	return w.record(ctx, *b, detail)
}

func (w *Worker) handle(item any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("dryrun failed: %T", recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	switch v := item.(type) {
	case capture.Progress:
		err = w.handleProgress(ctx, v)
	case capture.Rating:
		err = w.handleRating(ctx, v)
	case capture.SyncedBook:
		err = w.handleSynced(ctx, v)
	case retryPending:
		err = w.handleRetry(ctx)
	}
	if err != nil {
		log.Printf("dryrun: %v", err)
	}
}

func (w *Worker) handleSynced(ctx context.Context, s capture.SyncedBook) error {
	b, _, err := w.books.Get(ctx, s.EntitlementID)
	if err != nil {
		return err
	}
	b.EntitlementID, b.RevisionID = s.EntitlementID, s.RevisionID
	if !b.Resolved() {
		b.Title, b.Authors = s.Title, s.Authors
		if s.ISBN != "" {
			b.ISBN, b.ISBNFrom = s.ISBN, "sync"
		}
		b.Outcome = "" // more signals, so decide again on the next progress
	}
	return w.books.Put(ctx, b)
}

func (w *Worker) handleProgress(ctx context.Context, p capture.Progress) error {
	b, _, err := w.books.Get(ctx, p.EntitlementID)
	if err != nil {
		return err
	}
	b.EntitlementID = p.EntitlementID
	w.identify(ctx, &b, p)
	b.Percent, b.Status = p.Percent, p.Status
	b.Date = reportDate(p.LastModified)
	detail := nextAction(&b, p)
	if err := w.books.Put(ctx, b); err != nil {
		return err
	}
	if w.writer != nil && b.Resolved() && b.Shelf != "" {
		return w.write(ctx, &b)
	}
	return w.record(ctx, b, detail)
}

// reportDate falls back to today when the Kobo sends no usable time. Dates are
// UTC, so a late evening's read can land on the next day.
func reportDate(lastModified string) string {
	if t, err := time.Parse(time.RFC3339, lastModified); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	return time.Now().UTC().Format(time.DateOnly)
}

// identify keeps a result only once it's final, so a failed lookup or Hardcover
// call is tried again on the book's next progress.
func (w *Worker) identify(ctx context.Context, b *store.Book, p capture.Progress) {
	if b.Lookup != "found" && b.Lookup != "not_found" {
		meta, err := w.kobo.Metadata(ctx, p.EntitlementID, p.Headers)
		switch {
		case err == nil:
			b.Lookup = "found"
			b.RevisionID = meta.RevisionID
			b.Title, b.Authors = meta.Title, meta.Contributors
			if meta.ISBN != "" {
				b.ISBN, b.ISBNFrom = meta.ISBN, "store"
			}
			b.Outcome = ""
		case errors.Is(err, kobostore.ErrNotFound):
			b.Lookup = "not_found"
		default:
			b.Lookup = "error: " + err.Error()
		}
	}
	if b.ISBN == "" {
		if isbn := match.ISBNFromPath(p.Source); isbn != "" {
			b.ISBN, b.ISBNFrom = isbn, "bookmark"
		}
	}
	lookupFailed := b.Lookup != "found" && b.Lookup != "not_found"
	if b.Outcome != "" || w.lookup == nil || lookupFailed {
		return
	}
	decision, err := match.Decide(ctx, w.lookup, match.Signals{ISBN: b.ISBN, Title: b.Title, Authors: b.Authors})
	if err != nil {
		b.Reason = "hardcover error: " + err.Error()
		return
	}
	b.Outcome, b.HardcoverID, b.Reason, b.Suggestion = string(decision.Outcome), decision.BookID, decision.Reason, ""
	if len(decision.Candidates) > 0 {
		c := decision.Candidates[0]
		b.Suggestion = fmt.Sprintf("%d: %s", c.ID, c.Title)
	}
}

// nextAction applies the status rules: finished or 100% is read, above 1% is
// reading, and a book already read stays read.
func nextAction(b *store.Book, p capture.Progress) string {
	want := ""
	switch {
	case p.Status == "Finished" || p.Percent >= 100:
		want = shelfRead
	case p.Status == "Reading" && p.Percent > 1:
		want = shelfReading
	}
	if b.Shelf == shelfRead {
		want = shelfRead
	}
	if want != "" {
		b.Shelf = want
	}
	switch {
	case want == "":
		return fmt.Sprintf("no action at %d%%", p.Percent)
	case !b.Resolved():
		return fmt.Sprintf("%s at %d%%, not written: %s", want, p.Percent, unresolvedReason(*b))
	}
	return fmt.Sprintf("would set %s at %d%% on Hardcover book %d", want, p.Percent, b.HardcoverID)
}

func unresolvedReason(b store.Book) string {
	switch {
	case b.Outcome == "review":
		return "needs review (" + b.Reason + ")"
	case b.Reason != "":
		return b.Reason
	case b.Lookup != "found" && b.Lookup != "not_found":
		return "lookup " + b.Lookup
	}
	return "not matched yet"
}

func (w *Worker) handleRating(ctx context.Context, r capture.Rating) error {
	b, ok, err := w.books.ByRevision(ctx, r.ProductID)
	if err != nil {
		return err
	}
	if !ok {
		return w.books.AddEvent(ctx, store.Event{Time: time.Now(), EntitlementID: r.ProductID, Detail: fmt.Sprintf("%d stars for an unknown product, not written", r.Stars)})
	}
	b.Rating = r.Stars
	if err := w.books.Put(ctx, b); err != nil {
		return err
	}
	if w.writer != nil && b.Resolved() && b.Shelf != "" {
		return w.write(ctx, &b)
	}
	detail := fmt.Sprintf("%d stars, not written: %s", r.Stars, unresolvedReason(b))
	if b.Resolved() {
		detail = fmt.Sprintf("would rate %d stars on Hardcover book %d", r.Stars, b.HardcoverID)
	}
	return w.record(ctx, b, detail)
}

func (w *Worker) record(ctx context.Context, b store.Book, detail string) error {
	log.Printf("dryrun %s %q: %s", b.EntitlementID, b.Title, detail)
	return w.books.AddEvent(ctx, store.Event{Time: time.Now(), EntitlementID: b.EntitlementID, Title: b.Title, Detail: detail})
}
