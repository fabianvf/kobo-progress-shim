// Package cwasync makes a user's read status in CWA follow Hardcover, which is
// the source of truth, so the Kobo shows a book as finished when Hardcover has it
// as read. The Kobo learns of it through CWA's own sync. Nothing here touches the
// Kobo's traffic, and only CWA books can be covered, since the store's and Libby's
// books aren't in CWA.
package cwasync

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/dryrun"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

// Kobo reading statuses.
const (
	statusFinished = "Finished"
	statusReading  = "Reading"
	statusUnread   = "ReadyToRead"
)

// Markers saved in place of a status in dry-run, so each intent is logged once.
const (
	wouldFinish = "would finish"
	wouldUnread = "would unread"
)

// Hardcover is the read side the sync needs.
type Hardcover interface {
	match.Lookup
	Statuses(ctx context.Context, bookIDs []int) (map[int]int, error)
}

type Library interface {
	Books(ctx context.Context) ([]Book, error)
	State(ctx context.Context, token, uuid string) (State, error)
	SetStatus(ctx context.Context, token, uuid string, current State, status string) error
}

// Account is one user the sync runs for. Only an account with a Kobo token can
// be synced, because CWA's read status belongs to the user the token names.
type Account struct {
	Key       string
	KoboToken string
	Hardcover Hardcover
}

type Syncer struct {
	cwa      Library
	books    *store.Store
	accounts []Account
	dryRun   bool
	pause    time.Duration // between CWA calls, to stay out of the Kobo's way
}

func New(cwa Library, books *store.Store, accounts []Account, dryRun bool) *Syncer {
	return &Syncer{cwa: cwa, books: books, accounts: accounts, dryRun: dryRun, pause: 200 * time.Millisecond}
}

// Run syncs after delay and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, delay, interval time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.Once(ctx)
		timer.Reset(interval)
	}
}

// Once syncs every account. A failure is logged and the next run tries again.
func (s *Syncer) Once(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("cwa sync failed: %T", recovered)
		}
	}()
	for _, a := range s.accounts {
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		if err := s.account(runCtx, a); err != nil {
			log.Printf("cwa sync %s: %v", a.Key, err)
		}
		cancel()
	}
}

func (s *Syncer) account(ctx context.Context, a Account) error {
	library, err := s.cwa.Books(ctx)
	if err != nil {
		return err
	}
	var resolved []store.Book
	for _, cb := range library {
		b, err := s.identify(ctx, a, cb)
		if err != nil {
			return err
		}
		if b.Resolved() {
			resolved = append(resolved, b)
		}
	}
	var ids []int
	for _, b := range resolved {
		ids = append(ids, b.HardcoverID)
	}
	slices.Sort(ids)
	statuses, err := a.Hardcover.Statuses(ctx, slices.Compact(ids))
	if err != nil {
		return err
	}
	for _, b := range resolved {
		if err := s.follow(ctx, a, b, statuses[b.HardcoverID] == hardcover.StatusRead); err != nil {
			return err
		}
	}
	return nil
}

// identify matches a CWA book that has no match yet, the same way the worker
// does, so its decision shows in the review queue and serves the Kobo's reports
// too. A book already decided isn't searched again, review included.
func (s *Syncer) identify(ctx context.Context, a Account, cb Book) (store.Book, error) {
	var out store.Book
	err := s.books.Update(ctx, a.Key, cb.UUID, func(b *store.Book, ok bool) bool {
		b.User, b.EntitlementID = a.Key, cb.UUID
		defer func() { out = *b }()
		if b.Resolved() {
			return false
		}
		changed := !ok || b.Title != cb.Title || !slices.Equal(b.Authors, cb.Authors)
		if !changed && b.Outcome != "" {
			return false
		}
		if !ok {
			b.RevisionID = cb.UUID // CWA's books use one id for both
		}
		b.Title, b.Authors, b.Outcome = cb.Title, cb.Authors, ""
		dryrun.Decide(ctx, a.Hardcover, b)
		return true
	})
	return out, err
}

// follow sets Finished when Hardcover has the book as read, unless the Kobo is
// partway through it, which is a re-read. It only undoes a Finished it set itself,
// so a book finished on the Kobo or in CWA's web page is never unmarked.
func (s *Syncer) follow(ctx context.Context, a Account, b store.Book, read bool) error {
	st, err := s.books.CWAState(ctx, a.Key, b.EntitlementID)
	if err != nil {
		return err
	}
	switch {
	case read && st.Status != statusFinished:
		if s.dryRun {
			if st.Status == wouldFinish {
				return nil
			}
			st.Status = wouldFinish
			return s.save(ctx, a, b, st, "would mark finished in CWA, read on Hardcover")
		}
		current, err := s.state(ctx, a, b)
		if err != nil {
			return err
		}
		switch current.Status {
		case statusFinished:
			st.Status = statusFinished
			return s.books.SetCWAState(ctx, a.Key, b.EntitlementID, st)
		case statusReading:
			if st.Status == statusReading {
				return nil
			}
			st.Status = statusReading
			return s.save(ctx, a, b, st, "read on Hardcover but being read on the Kobo, left alone")
		}
		if err := s.cwa.SetStatus(ctx, a.KoboToken, b.EntitlementID, current, statusFinished); err != nil {
			return err
		}
		return s.save(ctx, a, b, store.CWAState{Status: statusFinished, Marked: true}, "marked finished in CWA, read on Hardcover")
	case !read && st.Marked && !b.Pending:
		if s.dryRun {
			if st.Status == wouldUnread {
				return nil
			}
			st.Status = wouldUnread
			return s.save(ctx, a, b, st, "would mark unread in CWA, not read on Hardcover")
		}
		current, err := s.state(ctx, a, b)
		if err != nil {
			return err
		}
		if current.Status != statusFinished {
			return s.books.SetCWAState(ctx, a.Key, b.EntitlementID, store.CWAState{Status: current.Status})
		}
		if err := s.cwa.SetStatus(ctx, a.KoboToken, b.EntitlementID, current, statusUnread); err != nil {
			return err
		}
		return s.save(ctx, a, b, store.CWAState{Status: statusUnread}, "marked unread in CWA, not read on Hardcover")
	}
	return nil
}

func (s *Syncer) state(ctx context.Context, a Account, b store.Book) (State, error) {
	select {
	case <-time.After(s.pause):
	case <-ctx.Done():
		return State{}, ctx.Err()
	}
	return s.cwa.State(ctx, a.KoboToken, b.EntitlementID)
}

func (s *Syncer) save(ctx context.Context, a Account, b store.Book, st store.CWAState, detail string) error {
	if err := s.books.SetCWAState(ctx, a.Key, b.EntitlementID, st); err != nil {
		return err
	}
	log.Printf("cwa sync %s %q: %s", b.EntitlementID, b.Title, detail)
	return s.books.AddEvent(ctx, store.Event{Time: time.Now(), User: a.Key, EntitlementID: b.EntitlementID, Title: b.Title,
		Detail: fmt.Sprintf("%s (Hardcover book %d)", detail, b.HardcoverID)})
}
