package cwasync

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
)

func TestNextInSeries(t *testing.T) {
	const today = "2026-11-11"
	book := func(id int, pos float64, lang, released string, readers int) hardcover.SeriesBook {
		return hardcover.SeriesBook{ID: id, Position: pos, Language: lang, Released: released, Readers: readers}
	}
	series := []hardcover.Series{
		// As Hardcover has the Iron Druid: a German-only 9, then 10 twice.
		{ID: 1, Books: []hardcover.SeriesBook{book(18, 8, "eng", "2016-01-01", 384), book(19, 9, "ger", "2017-07-11", 15),
			book(194, 9.4, "eng", "2019-02-28", 31), book(20, 10, "eng", "2018-01-01", 311), book(21, 10, "", "2018-04-03", 0)}},
		// The next one isn't out, so nothing, not the one after.
		{ID: 2, Books: []hardcover.SeriesBook{book(31, 1, "", "2010-01-01", 9), book(32, 2, "", "", 5), book(33, 3, "", "2030-01-01", 5)}},
		// Read out of order: the next is after the furthest read.
		{ID: 3, Books: []hardcover.SeriesBook{book(41, 1, "", "2001-01-01", 1), book(42, 2, "", "2002-01-01", 1), book(43, 3, "", "2003-01-01", 1)}},
		// Already wanted, though a duplicate has more readers.
		{ID: 4, Books: []hardcover.SeriesBook{book(51, 1, "", "2001-01-01", 1), book(52, 2, "", "2002-01-01", 1), book(53, 2, "", "2002-01-01", 99)}},
		// The next one was paused.
		{ID: 5, Books: []hardcover.SeriesBook{book(61, 1, "", "2001-01-01", 1), book(62, 2, "", "2002-01-01", 1)}},
		// Only a novella read, nothing numbered.
		{ID: 6, Books: []hardcover.SeriesBook{book(71, 0.5, "", "2001-01-01", 1), book(72, 1, "", "2002-01-01", 1)}},
		// Nothing read, only wanted.
		{ID: 7, Books: []hardcover.SeriesBook{book(81, 1, "", "2001-01-01", 1)}},
		// A split of the read book is no sequel, and the real one isn't out.
		{ID: 9, Books: []hardcover.SeriesBook{book(91, 1, "", "2005-01-01", 9), {ID: 92, Title: "Elantris (2 of 2)", Position: 2, Released: "2005-01-01"}}},
		// Same book as series 1's, under another series.
		{ID: 8, Books: []hardcover.SeriesBook{book(18, 1, "eng", "2016-01-01", 384), book(20, 2, "eng", "2018-01-01", 311)}},
	}
	status := map[int]int{18: 3, 31: 3, 41: 3, 42: 1, 43: 2, 51: 3, 52: 1, 61: 3, 62: 4, 71: 3, 81: 1, 91: 3}
	if got := nextInSeries(series, status, today); !slices.Equal(got, []int{20, 52}) {
		t.Fatalf("next = %v", got)
	}
	series[2].Books = append(series[2].Books, book(44, 4, "", "2004-01-01", 1))
	if got := nextInSeries(series, status, today); !slices.Equal(got, []int{20, 44, 52}) {
		t.Fatalf("next = %v", got)
	}
}

func TestNextUpWantsTheSequelAndKeepsACollection(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	hc.series = []hardcover.Series{
		// Summer Knight, already wanted, is next in the Dresden Files.
		{ID: 1, Name: "The Dresden Files", Books: []hardcover.SeriesBook{
			{ID: 1, Position: 2, Released: "2001-01-01"}, {ID: 2, Position: 3, Released: "2001-01-01"}, {ID: 3, Position: 4, Released: "2002-01-01"}}},
		// Book 10, on no shelf and not in the library, is next in another.
		{ID: 2, Name: "Other", Books: []hardcover.SeriesBook{{ID: 1, Position: 1, Released: "2001-01-01"}, {ID: 10, Title: "Sequel", Position: 2, Released: "2002-01-01"}}},
	}

	// Off, nothing is added and there's no collection.
	syncer(lib, s, hc, false).Once(ctx)
	if len(*hc.wanted) != 0 || lib.tags["tag-Next Up"] != nil {
		t.Fatalf("off: wanted %v, tags %v", *hc.wanted, lib.names)
	}

	s.SetNextUp(ctx, me, true)
	syncer(lib, s, hc, true).Once(ctx)
	if len(*hc.wanted) != 0 {
		t.Fatalf("dry run wanted %v", *hc.wanted)
	}
	syncer(lib, s, hc, false).Once(ctx)
	if !slices.Equal(*hc.wanted, []int{10}) {
		t.Fatalf("wanted = %v", *hc.wanted)
	}
	if got := lib.tags["tag-Next Up"]; !slices.Equal(got, []string{"u-unread"}) {
		t.Fatalf("Next Up = %v", got)
	}
	if got := lib.tags["tag-Want to Read"]; !slices.Equal(got, []string{"u-unread"}) {
		t.Fatalf("Want to Read = %v", got)
	}

	// Taken off Want to Read by the reader, it isn't put back.
	delete(hc.statuses, 10)
	syncer(lib, s, hc, false).Once(ctx)
	if len(*hc.wanted) != 1 {
		t.Fatalf("added again: %v", *hc.wanted)
	}

	// Turned off, the collection goes and Want to Read is left alone.
	s.SetNextUp(ctx, me, false)
	syncer(lib, s, hc, false).Once(ctx)
	if _, ok := lib.tags["tag-Next Up"]; ok {
		t.Fatal("Next Up kept after turning it off")
	}
	if !strings.Contains(strings.Join(lib.calls, ","), "delete Next Up") {
		t.Fatalf("calls = %v", lib.calls)
	}
}
