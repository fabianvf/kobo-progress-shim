package cwasync

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
)

// keyNextUp is the collection of the next book in each series the reader is
// reading, kept only for a reader who turned it on.
const keyNextUp = "next-up"

// Hardcover's statuses that end a series for the reader, beyond read.
const (
	statusPaused = 4
	statusDNF    = 5
)

// nextUp finds the next book in each series the reader is reading and, when
// they turned it on, puts any not on a shelf yet on Want to Read, so Want to
// Read downloads it as usual. Each book is added once, so one the reader takes
// off isn't put back. lib is read again after an add, for the collections and
// downloads that follow.
func (s *Syncer) nextUp(ctx context.Context, a Account, lib *hardcover.Library) (ids []int, on bool, err error) {
	if on, err = s.books.NextUp(ctx, a.Key); err != nil || !on {
		return nil, false, err
	}
	series, err := a.Hardcover.Series(ctx)
	if err != nil {
		return nil, true, err
	}
	ids = nextInSeries(series, lib.Status, time.Now().UTC().Format(time.DateOnly))
	added, err := s.books.NextUpAdded(ctx, a.Key)
	if err != nil {
		return nil, true, err
	}
	titles := map[int]string{}
	for _, sr := range series {
		for _, b := range sr.Books {
			titles[b.ID] = fmt.Sprintf("%s, %s #%g", b.Title, sr.Name, b.Position)
		}
	}
	changed := false
	for _, id := range ids {
		if _, shelved := lib.Status[id]; shelved || added[id] {
			continue
		}
		if s.dryRun {
			log.Printf("cwa sync %s: would add %s to Want to Read, next up", a.Key, titles[id])
			continue
		}
		if err := a.Hardcover.WantToRead(ctx, id); err != nil {
			return nil, true, err
		}
		if err := s.books.AddNextUp(ctx, a.Key, id); err != nil {
			return nil, true, err
		}
		changed = true
		s.note(ctx, a, fmt.Sprintf("added %s to Want to Read on Hardcover, next up (Hardcover book %d)", titles[id], id))
	}
	if changed {
		if *lib, err = a.Hardcover.Library(ctx); err != nil {
			return nil, true, err
		}
	}
	return ids, true, nil
}

// split is a part of one book sold separately, like "Elantris (2 of 2)", which
// Hardcover can list as the next in a series.
var split = regexp.MustCompile(`\(\d+ of \d+\)`)

// nextInSeries is the next book in each series with a book read or being read:
// the first numbered entry after the furthest one the reader has, skipping
// novellas, which have fractional positions, and parts of a split book. Among
// editions at that place, a translation into a language the reader hasn't read
// the series in is skipped, and the one with the most Hardcover readers is
// taken. A series whose next book isn't out yet, or which the reader paused or
// didn't finish, has none.
func nextInSeries(series []hardcover.Series, status map[int]int, today string) []int {
	var out []int
	for _, sr := range series {
		last, langs := -1.0, map[string]bool{}
		for _, b := range sr.Books {
			if st := status[b.ID]; st == hardcover.StatusRead || st == statusReadingID {
				if b.Position == math.Trunc(b.Position) {
					last = max(last, b.Position)
				}
				if b.Language != "" {
					langs[b.Language] = true
				}
			}
		}
		if last < 0 {
			continue
		}
		next, at := []hardcover.SeriesBook(nil), 0.0
		for _, b := range sr.Books {
			if b.Position <= last || b.Position != math.Trunc(b.Position) || split.MatchString(b.Title) ||
				(b.Language != "" && len(langs) > 0 && !langs[b.Language]) {
				continue
			}
			if next != nil && b.Position != at {
				break
			}
			next, at = append(next, b), b.Position
		}
		if pick, ok := choose(next, status, today); ok {
			out = append(out, pick)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func choose(candidates []hardcover.SeriesBook, status map[int]int, today string) (int, bool) {
	best, ok := hardcover.SeriesBook{}, false
	for _, b := range candidates {
		switch status[b.ID] {
		case statusPaused, statusDNF:
			return 0, false
		case hardcover.StatusWantToRead:
			return b.ID, true
		case 0:
			if b.Released != "" && b.Released <= today && (!ok || b.Readers > best.Readers) {
				best, ok = b, true
			}
		}
	}
	return best.ID, ok
}
