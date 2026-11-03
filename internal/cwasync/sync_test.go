package cwasync

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

type fakeLibrary struct {
	books  []Book
	states map[string]State
	sets   []string
	reads  int
}

func (f *fakeLibrary) Books(context.Context) ([]Book, error) { return f.books, nil }
func (f *fakeLibrary) State(_ context.Context, _, uuid string) (State, error) {
	f.reads++
	return f.states[uuid], nil
}
func (f *fakeLibrary) SetStatus(_ context.Context, _, uuid string, _ State, status string) error {
	f.sets = append(f.sets, uuid+"="+status)
	f.states[uuid] = State{Status: status}
	return nil
}

type fakeHardcover struct {
	books    map[string]match.Book // by title
	statuses map[int]int
}

func (f fakeHardcover) BooksByISBN(context.Context, string) ([]match.Book, error) { return nil, nil }
func (f fakeHardcover) Search(_ context.Context, title, _ string) ([]match.Book, error) {
	if b, ok := f.books[title]; ok {
		return []match.Book{b}, nil
	}
	return nil, nil
}
func (f fakeHardcover) Statuses(context.Context, []int) (map[int]int, error) { return f.statuses, nil }

const me = "me"

func setup(t *testing.T) (*store.Store, *fakeLibrary, fakeHardcover, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s, err := store.Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	lib := &fakeLibrary{
		books: []Book{
			{UUID: "u-read", Title: "Fool Moon", Authors: []string{"Jim Butcher"}},
			{UUID: "u-reading", Title: "Grave Peril", Authors: []string{"Jim Butcher"}},
			{UUID: "u-unread", Title: "Summer Knight", Authors: []string{"Jim Butcher"}},
			{UUID: "u-unknown", Title: "Untitled 6619", Authors: []string{"Penguin Random House"}},
		},
		states: map[string]State{"u-reading": {Status: statusReading}},
	}
	hc := fakeHardcover{
		books: map[string]match.Book{
			"Fool Moon":     {ID: 1, Title: "Fool Moon", Authors: []string{"Jim Butcher"}},
			"Grave Peril":   {ID: 2, Title: "Grave Peril", Authors: []string{"Jim Butcher"}},
			"Summer Knight": {ID: 3, Title: "Summer Knight", Authors: []string{"Jim Butcher"}},
		},
		statuses: map[int]int{1: 3, 2: 3, 3: 1},
	}
	return s, lib, hc, &logged
}

func TestFinishesWhatHardcoverHasAsRead(t *testing.T) {
	s, lib, hc, logged := setup(t)
	sync := New(lib, s, []Account{{Key: me, KoboToken: "sekrit", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(context.Background())
	if strings.Join(lib.sets, ",") != "u-read=Finished" {
		t.Fatalf("sets = %v", lib.sets)
	}
	unknown, ok, _ := s.Get(context.Background(), me, "u-unknown")
	if !ok || unknown.Outcome != string(match.Review) {
		t.Errorf("unmatched book = %+v", unknown)
	}

	// A second run reads only the book being read, and changes nothing.
	lib.reads = 0
	sync.Once(context.Background())
	if len(lib.sets) != 1 || lib.reads != 1 {
		t.Fatalf("second run: sets %v, reads %d", lib.sets, lib.reads)
	}
	events, _ := s.RecentEvents(context.Background(), 10)
	if len(events) != 2 {
		t.Errorf("events = %+v", events)
	}
	if strings.Contains(logged.String(), "sekrit") {
		t.Error("token logged")
	}
}

func TestUndoesOnlyItsOwnFinished(t *testing.T) {
	s, lib, hc, _ := setup(t)
	lib.states["u-unread"] = State{Status: statusFinished} // finished on the Kobo
	sync := New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(context.Background())

	hc.statuses = map[int]int{} // both taken off Hardcover's read shelf
	sync = New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(context.Background())
	if strings.Join(lib.sets, ",") != "u-read=Finished,u-read=ReadyToRead" {
		t.Fatalf("sets = %v", lib.sets)
	}
	if lib.states["u-unread"].Status != statusFinished {
		t.Error("unmarked a book the Kobo finished")
	}
}

func TestPendingKoboFinishIsNotUndone(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	sync := New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(ctx)
	// The Kobo reports it finished but Hardcover hasn't got the write yet.
	s.Update(ctx, me, "u-read", func(b *store.Book, _ bool) bool { b.Pending = true; return true })
	hc.statuses = map[int]int{}
	sync = New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(ctx)
	if len(lib.sets) != 1 {
		t.Fatalf("sets = %v", lib.sets)
	}
}

func TestDryRunCallsNothingThenLiveActs(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	dry := New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, true)
	dry.Once(ctx)
	dry.Once(ctx)
	if lib.reads != 0 || len(lib.sets) != 0 {
		t.Fatalf("dry run: reads %d, sets %v", lib.reads, lib.sets)
	}
	events, _ := s.RecentEvents(ctx, 10)
	if len(events) != 2 || !strings.Contains(events[0].Detail, "would") {
		t.Fatalf("events = %+v", events)
	}
	live := New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	live.pause = 0
	live.Once(ctx)
	if strings.Join(lib.sets, ",") != "u-read=Finished" {
		t.Fatalf("live sets = %v", lib.sets)
	}
}

func TestHandChosenMatchIsKept(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	s.Put(ctx, store.Book{User: me, EntitlementID: "u-unknown", Outcome: "review"})
	s.Confirm(ctx, me, "u-unknown", 3)
	sync := New(lib, s, []Account{{Key: me, KoboToken: "t", Hardcover: hc}}, false)
	sync.pause = 0
	sync.Once(ctx)
	b, _, _ := s.Get(ctx, me, "u-unknown")
	if b.HardcoverID != 3 || b.Outcome != store.Confirmed || b.Title != "" {
		t.Fatalf("book = %+v", b)
	}
}
