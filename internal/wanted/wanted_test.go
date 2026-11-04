package wanted

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

type fakeWorks struct {
	byISBN map[string]Work
	search []Work
}

func (f fakeWorks) ByISBN(_ context.Context, isbn string) (Work, bool, error) {
	w, ok := f.byISBN[isbn]
	return w, ok, nil
}
func (f fakeWorks) Search(context.Context, string, string) ([]Work, error) { return f.search, nil }

type fakeLL struct {
	statuses map[string]string
	added    []string
	queued   []string
	renumber bool // add Hardcover-source books under some other id
}

func (f *fakeLL) Statuses(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range f.statuses {
		out[k] = v
	}
	return out, nil
}
func (f *fakeLL) Add(_ context.Context, id, source string) error {
	f.added = append(f.added, id+"@"+source)
	if f.renumber && source == sourceHardcover {
		id = "other-" + id
	}
	f.statuses[id] = "Skipped"
	return nil
}
func (f *fakeLL) Queue(_ context.Context, id string) error {
	f.queued = append(f.queued, id)
	f.statuses[id] = "Wanted"
	return nil
}

func setup(t *testing.T) *store.Store {
	t.Helper()
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s, err := store.Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var (
	carl  = hardcover.ShelfBook{ID: 446681, Title: "Dungeon Crawler Carl", Authors: []string{"Matt Dinniman"}, ISBNs: []string{"9798217287161"}, Language: "eng"}
	red   = hardcover.ShelfBook{ID: 427971, Title: "All Systems Red", Authors: []string{"Martha Wells"}, Language: "eng"}
	owned = hardcover.ShelfBook{ID: 1, Title: "Elantris", Authors: []string{"Brandon Sanderson"}}
)

func library(books ...hardcover.ShelfBook) hardcover.Library {
	lib := hardcover.Library{Status: map[int]int{}, Books: map[int]hardcover.ShelfBook{}}
	for _, b := range books {
		lib.Status[b.ID] = hardcover.StatusWantToRead
		lib.Books[b.ID] = b
	}
	return lib
}

func TestRequestsOnceThroughOpenLibrary(t *testing.T) {
	s := setup(t)
	works := fakeWorks{
		byISBN: map[string]Work{"9798217287161": {ID: "OL24593432W", Title: "Dungeon Crawler Carl", Authors: []string{"Matt Dinniman"}}},
		search: []Work{{ID: "OL17357945W", Title: "All Systems Red", Authors: []string{"Martha Wells"}}},
	}
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(works, ll, s, false, false)
	lib := library(carl, red, owned)
	for range 2 {
		if err := d.Want(context.Background(), "me", lib, map[int]bool{1: true}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(ll.queued, ",") != "OL17357945W,OL24593432W" || len(ll.added) != 2 {
		t.Fatalf("added %v, queued %v", ll.added, ll.queued)
	}
	r, ok, _ := s.Request(context.Background(), carl.ID)
	if !ok || r.State != stateQueued || r.WorkID != "OL24593432W" || r.Users[0] != "me" {
		t.Fatalf("request = %+v", r)
	}
}

func TestAlreadyInLazyLibrarianIsNotQueuedAgain(t *testing.T) {
	s := setup(t)
	works := fakeWorks{byISBN: map[string]Work{"9798217287161": {ID: "OL24593432W", Title: "Dungeon Crawler Carl", Authors: []string{"Matt Dinniman"}}}}
	ll := &fakeLL{statuses: map[string]string{"OL24593432W": "Open"}}
	d := New(works, ll, s, false, false)
	if err := d.Want(context.Background(), "me", library(carl), nil); err != nil {
		t.Fatal(err)
	}
	if len(ll.queued) != 0 || len(ll.added) != 0 {
		t.Fatalf("added %v, queued %v", ll.added, ll.queued)
	}
}

func TestDisagreeingWorkGoesToReview(t *testing.T) {
	s := setup(t)
	works := fakeWorks{
		byISBN: map[string]Work{"9798217287161": {ID: "OLX", Title: "Dungeon Crawler Carl: The Collection", Authors: []string{"Someone Else"}}},
		search: []Work{{ID: "OLY", Title: "Carl", Authors: []string{"Matt Dinniman"}}},
	}
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(works, ll, s, false, false)
	d.Want(context.Background(), "me", library(carl), nil)
	if len(ll.queued) != 0 {
		t.Fatalf("queued %v", ll.queued)
	}
	if r, _, _ := s.Request(context.Background(), carl.ID); r.State != stateReview {
		t.Fatalf("request = %+v", r)
	}
}

func TestNotFoundIsRetriedWeekly(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(fakeWorks{}, ll, s, false, false)
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	d.Want(context.Background(), "me", library(red), nil)
	d.works = fakeWorks{search: []Work{{ID: "OL1W", Title: "All Systems Red", Authors: []string{"Martha Wells"}}}}
	d.Want(context.Background(), "me", library(red), nil)
	if len(ll.queued) != 0 {
		t.Fatal("retried within the week")
	}
	now = now.Add(8 * 24 * time.Hour)
	d.Want(context.Background(), "me", library(red), nil)
	if strings.Join(ll.queued, ",") != "OL1W" {
		t.Fatalf("queued %v", ll.queued)
	}
}

func TestHardcoverSourceUsesTheHardcoverID(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(fakeWorks{}, ll, s, true, false)
	d.Want(context.Background(), "me", library(red), nil)
	if strings.Join(ll.added, ",") != "427971@HardCover" || strings.Join(ll.queued, ",") != "427971" {
		t.Fatalf("added %v, queued %v", ll.added, ll.queued)
	}

	other := setup(t)
	ll = &fakeLL{statuses: map[string]string{}, renumber: true}
	d = New(fakeWorks{}, ll, other, true, false)
	d.Want(context.Background(), "me", library(red), nil)
	if len(ll.queued) != 0 {
		t.Fatalf("queued an id LazyLibrarian doesn't have: %v", ll.queued)
	}
	if r, _, _ := other.Request(context.Background(), red.ID); r.State != stateReview {
		t.Fatalf("request = %+v", r)
	}
}

func TestDryRunAddsNothingThenLiveDoes(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	dry := New(fakeWorks{}, ll, s, true, true)
	dry.Want(context.Background(), "me", library(red), nil)
	if len(ll.added) != 0 {
		t.Fatalf("dry run added %v", ll.added)
	}
	live := New(fakeWorks{}, ll, s, true, false)
	live.Want(context.Background(), "me", library(red), nil)
	if len(ll.queued) != 1 {
		t.Fatalf("live queued %v", ll.queued)
	}
}

func TestForeignEditionIsSkipped(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	b := red
	b.Language = "deu"
	New(fakeWorks{}, ll, s, true, false).Want(context.Background(), "me", library(b), nil)
	if len(ll.added) != 0 {
		t.Fatalf("added %v", ll.added)
	}
}

func TestOpenLibraryIsTheFallback(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}, renumber: true} // the Hardcover add never lands
	works := fakeWorks{search: []Work{{ID: "OL1W", Title: "All Systems Red", Authors: []string{"Martha Wells"}}}}
	d := New(works, ll, s, true, false)
	d.Want(context.Background(), "me", library(red), nil)
	if strings.Join(ll.added, ",") != "427971@HardCover,427971@HardCover,OL1W@" || strings.Join(ll.queued, ",") != "OL1W" {
		t.Fatalf("added %v, queued %v", ll.added, ll.queued)
	}
	if r, _, _ := s.Request(context.Background(), red.ID); r.State != stateQueued || r.WorkID != "OL1W" {
		t.Fatalf("request = %+v", r)
	}
}

type flakyWorks struct{ fakeWorks }

func (f flakyWorks) Search(_ context.Context, title, _ string) ([]Work, error) {
	if title == "All Systems Red" {
		return nil, errors.New("openlibrary: status 500")
	}
	return f.fakeWorks.Search(context.Background(), title, "")
}

func TestOneFailureDoesntStopTheRest(t *testing.T) {
	s := setup(t)
	works := flakyWorks{fakeWorks{byISBN: map[string]Work{"9798217287161": {ID: "OL24593432W", Title: "Dungeon Crawler Carl", Authors: []string{"Matt Dinniman"}}}}}
	ll := &fakeLL{statuses: map[string]string{}}
	err := New(works, ll, s, false, false).Want(context.Background(), "me", library(red, carl), nil)
	if err == nil || strings.Join(ll.queued, ",") != "OL24593432W" {
		t.Fatalf("err %v, queued %v", err, ll.queued)
	}
	if r, _, _ := s.Request(context.Background(), red.ID); r.State != "" || r.Failures != 1 {
		t.Errorf("the failed book was settled, so it won't be retried: %+v", r)
	}
}

func TestABookThatKeepsFailingIsTriedWeekly(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(flakyWorks{}, ll, s, false, false)
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	for range maxFailures {
		if err := d.Want(context.Background(), "me", library(red), nil); err == nil {
			t.Fatal("no error")
		}
	}
	r, _, _ := s.Request(context.Background(), red.ID)
	if r.State != stateNotFound || r.Failures != 0 || !strings.Contains(r.Detail, "status 500") {
		t.Fatalf("request = %+v", r)
	}
	if err := d.Want(context.Background(), "me", library(red), nil); err != nil {
		t.Fatalf("tried again within the week: %v", err)
	}
	d.works = fakeWorks{search: []Work{{ID: "OL1W", Title: "All Systems Red", Authors: []string{"Martha Wells"}}}}
	now = now.Add(8 * 24 * time.Hour)
	d.Want(context.Background(), "me", library(red), nil)
	if strings.Join(ll.queued, ",") != "OL1W" {
		t.Fatalf("queued %v", ll.queued)
	}
}

func TestWipedLazyLibrarianIsAskedAgain(t *testing.T) {
	s := setup(t)
	ll := &fakeLL{statuses: map[string]string{}}
	d := New(fakeWorks{}, ll, s, true, false)
	d.Want(context.Background(), "me", library(red), nil)
	ll.statuses = map[string]string{} // LazyLibrarian wiped
	d.Want(context.Background(), "me", library(red), nil)
	if strings.Join(ll.queued, ",") != "427971,427971" {
		t.Fatalf("queued %v", ll.queued)
	}
	d.Want(context.Background(), "me", library(red), nil)
	if len(ll.queued) != 2 {
		t.Fatalf("asked again while LazyLibrarian still has it: %v", ll.queued)
	}
}
