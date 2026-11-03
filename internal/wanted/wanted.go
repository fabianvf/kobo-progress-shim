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
	hardcover bool // add by Hardcover id through LazyLibrarian's Hardcover source
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
	for _, id := range ids {
		if err := d.request(ctx, user, lib.Books[id], statuses); err != nil {
			return err
		}
	}
	return nil
}

func (d *Downloader) request(ctx context.Context, user string, book hardcover.ShelfBook, statuses map[string]string) error {
	r, ok, err := d.books.Request(ctx, book.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(r.Users, user) {
		r.Users = append(r.Users, user)
		if ok {
			if err := d.books.PutRequest(ctx, r); err != nil {
				return err
			}
		}
	}
	if ok && !(r.State == stateNotFound && d.now().Sub(r.Updated) > retryAfter) && !(r.State == stateWould && !d.dryRun) {
		return nil
	}
	r.HardcoverID, r.Title = book.ID, book.Title
	if book.Language != "" && book.Language != "eng" {
		return d.save(ctx, user, r, stateForeign, "its ebook edition isn't in English")
	}

	id, source := strconv.Itoa(book.ID), sourceHardcover
	if !d.hardcover {
		work, state, detail, err := d.work(ctx, book)
		if err != nil {
			return err
		}
		if state != "" {
			return d.save(ctx, user, r, state, detail)
		}
		id, source, r.WorkID = work.ID, "", work.ID
	}
	switch statuses[id] {
	case "Open", "Have", "Wanted", "Snatched", "Processed":
		return d.save(ctx, user, r, stateHave, "LazyLibrarian already has it "+statuses[id])
	}
	if d.dryRun {
		return d.save(ctx, user, r, stateWould, "would add to LazyLibrarian as "+id)
	}
	if _, known := statuses[id]; !known {
		if err := d.ll.Add(ctx, id, source); err != nil {
			return err
		}
		// Unverified that a book from the Hardcover source keeps the Hardcover
		// id as its BookID, so check before queueing something that isn't there.
		after, err := d.ll.Statuses(ctx)
		if err != nil {
			return err
		}
		if _, added := after[id]; !added {
			return d.save(ctx, user, r, stateReview, "LazyLibrarian added it under an id other than "+id)
		}
		statuses[id] = after[id]
	}
	if err := d.ll.Queue(ctx, id); err != nil {
		return err
	}
	return d.save(ctx, user, r, stateQueued, "wanted in LazyLibrarian as "+id)
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

func (d *Downloader) save(ctx context.Context, user string, r store.Request, state, detail string) error {
	r.State, r.Detail, r.Updated = state, detail, d.now()
	if err := d.books.PutRequest(ctx, r); err != nil {
		return err
	}
	log.Printf("wanted %d %q: %s, %s", r.HardcoverID, r.Title, state, detail)
	return d.books.AddEvent(ctx, store.Event{Time: d.now(), User: user, EntitlementID: "hardcover:" + strconv.Itoa(r.HardcoverID),
		Title: r.Title, Detail: "Want to Read: " + state + ", " + detail})
}
