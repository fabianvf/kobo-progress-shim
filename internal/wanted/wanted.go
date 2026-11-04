// Package wanted downloads what people want to read. A book on someone's
// Hardcover Want to Read that the library doesn't have goes to LazyLibrarian,
// one book at a time, never a whole author. Each book is requested once, and a
// person sees on the activity page what couldn't be matched.
package wanted

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

// Request states. Only notFound is tried again, after retryAfter, because a
// book may be added to OpenLibrary later.
const (
	stateQueued   = "queued" // LazyLibrarian has it Wanted
	stateHave     = "have"   // LazyLibrarian already had it, downloaded or wanted
	stateReview   = "review" // the match wasn't certain, a person decides
	stateNotFound = "not found"
	stateForeign  = "not english"
	stateWould    = "would request" // dry run
)

const retryAfter = 7 * 24 * time.Hour

// maxFailures is how many runs in a row a book may fail before it's saved as
// not found, and so tried weekly instead of hourly.
const maxFailures = 3

// sourceHardcover is LazyLibrarian's name for its Hardcover book source.
const sourceHardcover = "HardCover"

type Works interface {
	ByISBN(ctx context.Context, isbn string) (Work, bool, error)
	Search(ctx context.Context, title, author string) ([]Work, error)
}

type Library interface {
	Statuses(ctx context.Context) (map[string]string, error)
	Add(ctx context.Context, id, source string) error
	Queue(ctx context.Context, id string) error
}

type Downloader struct {
	works     Works
	ll        Library
	books     *store.Store
	dryRun    bool
	hardcover bool // LazyLibrarian's Hardcover source first, by Hardcover id
	now       func() time.Time
}

func New(works Works, ll Library, books *store.Store, viaHardcover, dryRun bool) *Downloader {
	return &Downloader{works: works, ll: ll, books: books, dryRun: dryRun, hardcover: viaHardcover, now: time.Now}
}

// Want requests every book on user's Want to Read that have doesn't contain.
// have is the Hardcover ids of books the library already holds.
func (d *Downloader) Want(ctx context.Context, user string, lib hardcover.Library, have map[int]bool) error {
	var ids []int
	for id, status := range lib.Status {
		if status == hardcover.StatusWantToRead && !have[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	statuses, err := d.ll.Statuses(ctx)
	if err != nil {
		return err
	}
	// One book's failure, such as OpenLibrary answering 500, is logged and the
	// rest go on. The book is tried again next run, up to maxFailures runs.
	failed := 0
	for _, id := range ids {
		if err := d.request(ctx, user, lib.Books[id], statuses); err != nil {
			if ctx.Err() != nil {
				return err
			}
			failed++
			log.Printf("wanted %d %q: %v, will retry", id, lib.Books[id].Title, err)
			if err := d.failure(ctx, user, lib.Books[id], err); err != nil {
				return err
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("wanted: %d of %d books failed, will retry", failed, len(ids))
	}
	return nil
}

func (d *Downloader) request(ctx context.Context, user string, book hardcover.ShelfBook, statuses map[string]string) error {
	r, ok, err := d.books.Request(ctx, book.ID)
	if err != nil {
		return err
	}
	// Authors came later, and the CWA sync needs them to match an import back
	// to its request.
	if !slices.Contains(r.Users, user) || (ok && len(r.Authors) == 0 && len(book.Authors) > 0) {
		if !slices.Contains(r.Users, user) {
			r.Users = append(r.Users, user)
		}
		r.Authors = book.Authors
		if ok {
			if err := d.books.PutRequest(ctx, r); err != nil {
				return err
			}
		}
	}
	unmatched := r.State == stateNotFound || r.State == stateReview
	// A book LazyLibrarian no longer holds, because it was wiped or the book
	// unqueued, is asked for again.
	llID := r.LLID
	if llID == "" {
		llID = r.WorkID
	}
	gone := (r.State == stateQueued || r.State == stateHave) && llID != "" && !wantedIn(statuses[llID])
	retry := gone || r.State == "" || // it failed before it settled
		(r.State == stateNotFound && d.now().Sub(r.Updated) > retryAfter) ||
		(r.State == stateWould && !d.dryRun) ||
		(unmatched && d.hardcover && !r.TriedHardcover) // the fallback came after
	if ok && !retry {
		return nil
	}
	r.HardcoverID, r.Title, r.Authors = book.ID, book.Title, book.Authors
	if book.Language != "" && book.Language != "eng" {
		return d.save(ctx, user, r, stateForeign, "its ebook edition isn't in English")
	}

	// With LazyLibrarian on its Hardcover source, the Hardcover id is the book id
	// everywhere, so it's tried first. OpenLibrary is the way otherwise, and the
	// fallback when LazyLibrarian won't add a book from Hardcover.
	if d.hardcover {
		r.TriedHardcover = true
		queued, err := d.add(ctx, user, &r, strconv.Itoa(book.ID), sourceHardcover, statuses)
		if err != nil || queued {
			return err
		}
	}
	work, state, detail, err := d.work(ctx, book)
	if err != nil {
		return err
	}
	if state != "" {
		if d.hardcover { // neither way worked, so a person should look
			state, detail = stateReview, "LazyLibrarian didn't add it from Hardcover, and OpenLibrary: "+detail
		}
		return d.save(ctx, user, r, state, detail)
	}
	r.WorkID = work.ID
	queued, err := d.add(ctx, user, &r, work.ID, "", statuses)
	if err != nil || queued {
		return err
	}
	return d.save(ctx, user, r, stateReview, "LazyLibrarian didn't add it as "+work.ID)
}

// add queues id in LazyLibrarian, adding it first if LazyLibrarian doesn't have
// it. It reports whether the request is settled, saved as have, would or queued.
func (d *Downloader) add(ctx context.Context, user string, r *store.Request, id, source string, statuses map[string]string) (bool, error) {
	if wantedIn(statuses[id]) {
		r.LLID = id
		return true, d.save(ctx, user, *r, stateHave, "LazyLibrarian already has it "+statuses[id])
	}
	if d.dryRun {
		return true, d.save(ctx, user, *r, stateWould, "would add to LazyLibrarian as "+id)
	}
	if _, known := statuses[id]; !known {
		// From the Hardcover source, LazyLibrarian's first add for a new author
		// only adds the author, and a second adds the book (seen 2026-11-03), so
		// it gets two tries. Its BookID is then the Hardcover id.
		added := false
		for attempt := 0; attempt < 2 && !added; attempt++ {
			if err := d.ll.Add(ctx, id, source); err != nil {
				return false, err
			}
			after, err := d.ll.Statuses(ctx)
			if err != nil {
				return false, err
			}
			if _, added = after[id]; added {
				statuses[id] = after[id]
			}
		}
		if !added {
			return false, nil
		}
	}
	if err := d.ll.Queue(ctx, id); err != nil {
		return false, err
	}
	r.LLID = id
	return true, d.save(ctx, user, *r, stateQueued, "wanted in LazyLibrarian as "+id)
}

// wantedIn reports whether a LazyLibrarian status means it has or will get it.
func wantedIn(status string) bool {
	switch status {
	case "Open", "Have", "Wanted", "Snatched", "Processed":
		return true
	}
	return false
}

// work finds the OpenLibrary work for a book: by an ISBN whose work agrees on
// title and author, else by exactly one search result that does.
func (d *Downloader) work(ctx context.Context, book hardcover.ShelfBook) (Work, string, string, error) {
	agrees := func(w Work) bool {
		return match.Same(book.Title, book.Authors, match.Book{Title: w.Title, Authors: w.Authors})
	}
	for _, isbn := range book.ISBNs {
		w, found, err := d.works.ByISBN(ctx, isbn)
		if err != nil {
			return Work{}, "", "", err
		}
		if found && agrees(w) {
			return w, "", "", nil
		}
	}
	author := ""
	if len(book.Authors) > 0 {
		author = book.Authors[0]
	}
	found, err := d.works.Search(ctx, book.Title, author)
	if err != nil {
		return Work{}, "", "", err
	}
	var exact []Work
	for _, w := range found {
		if agrees(w) {
			exact = append(exact, w)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], "", "", nil
	case len(exact) > 1:
		return Work{}, stateReview, fmt.Sprintf("%d OpenLibrary works match, first %s", len(exact), exact[0].ID), nil
	case len(found) > 0:
		return Work{}, stateReview, fmt.Sprintf("no OpenLibrary work agrees, nearest %s %q", found[0].ID, found[0].Title), nil
	}
	return Work{}, stateNotFound, "not on OpenLibrary", nil
}

// failure counts a run that failed for book. After maxFailures in a row it's
// saved as not found, which shows on the activity page and is tried weekly.
func (d *Downloader) failure(ctx context.Context, user string, book hardcover.ShelfBook, cause error) error {
	r, _, err := d.books.Request(ctx, book.ID)
	if err != nil {
		return err
	}
	r.HardcoverID, r.Title, r.Authors = book.ID, book.Title, book.Authors
	if !slices.Contains(r.Users, user) {
		r.Users = append(r.Users, user)
	}
	if r.Failures++; r.Failures < maxFailures {
		return d.books.PutRequest(ctx, r)
	}
	return d.save(ctx, user, r, stateNotFound, fmt.Sprintf("failed %d runs in a row, trying again in a week: %v", r.Failures, cause))
}

func (d *Downloader) save(ctx context.Context, user string, r store.Request, state, detail string) error {
	r.State, r.Detail, r.Updated, r.Failures = state, detail, d.now(), 0
	if err := d.books.PutRequest(ctx, r); err != nil {
		return err
	}
	log.Printf("wanted %d %q: %s, %s", r.HardcoverID, r.Title, state, detail)
	return d.books.AddEvent(ctx, store.Event{Time: d.now(), User: user, EntitlementID: "hardcover:" + strconv.Itoa(r.HardcoverID),
		Title: r.Title, Detail: "Want to Read: " + state + ", " + detail})
}
