package cwasync

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

type fakeLibrary struct {
	books    []Book
	states   map[string]State
	sets     []string
	reads    int
	percents map[string]float64
	tags     map[string][]string // tag id to items
	names    map[string]string   // tag id to name
	calls    []string
}

func (f *fakeLibrary) CreateTag(_ context.Context, _, name string, items []string) (string, error) {
	if f.tags == nil {
		f.tags, f.names = map[string][]string{}, map[string]string{}
	}
	for id, n := range f.names {
		if n == name {
			f.tags[id] = append(f.tags[id], items...)
			return id, nil
		}
	}
	id := "tag-" + name
	f.tags[id], f.names[id] = append([]string{}, items...), name
	f.calls = append(f.calls, "create "+name)
	return id, nil
}
func (f *fakeLibrary) RenameTag(_ context.Context, _, id, name string) error {
	if _, ok := f.tags[id]; !ok {
		return ErrGone
	}
	f.names[id] = name
	f.calls = append(f.calls, "rename "+name)
	return nil
}
func (f *fakeLibrary) AddToTag(_ context.Context, _, id string, items []string) error {
	if _, ok := f.tags[id]; !ok {
		return ErrGone
	}
	f.tags[id] = append(f.tags[id], items...)
	f.calls = append(f.calls, "add "+f.names[id])
	return nil
}
func (f *fakeLibrary) RemoveFromTag(_ context.Context, _, id string, items []string) error {
	if _, ok := f.tags[id]; !ok {
		return ErrGone
	}
	var kept []string
	for _, u := range f.tags[id] {
		if !slices.Contains(items, u) {
			kept = append(kept, u)
		}
	}
	f.tags[id] = kept
	f.calls = append(f.calls, "remove "+f.names[id])
	return nil
}
func (f *fakeLibrary) DeleteTag(_ context.Context, _, id string) error {
	if _, ok := f.tags[id]; !ok {
		return ErrGone
	}
	f.calls = append(f.calls, "delete "+f.names[id])
	delete(f.tags, id)
	delete(f.names, id)
	return nil
}

func (f *fakeLibrary) Books(context.Context) ([]Book, error) { return f.books, nil }
func (f *fakeLibrary) State(_ context.Context, _, uuid string) (State, error) {
	f.reads++
	return f.states[uuid], nil
}
func (f *fakeLibrary) SetStatus(_ context.Context, _, uuid string, current State, status string) error {
	f.sets = append(f.sets, uuid+"="+status)
	if f.percents == nil {
		f.percents = map[string]float64{}
	}
	if current.Percent != nil {
		f.percents[uuid] = *current.Percent
	}
	f.states[uuid] = State{Status: status, Percent: current.Percent}
	return nil
}

type fakeHardcover struct {
	books    map[string]match.Book // by title
	statuses map[int]int
	progress map[int]float64
	lists    []hardcover.List
}

func (f fakeHardcover) BooksByISBN(context.Context, string) ([]match.Book, error) { return nil, nil }
func (f fakeHardcover) Search(_ context.Context, title, _ string) ([]match.Book, error) {
	if b, ok := f.books[title]; ok {
		return []match.Book{b}, nil
	}
	return nil, nil
}
func (f fakeHardcover) Library(context.Context) (hardcover.Library, error) {
	return hardcover.Library{Status: f.statuses, Progress: f.progress, Lists: f.lists}, nil
}

func syncer(lib *fakeLibrary, s *store.Store, hc fakeHardcover, dryRun bool) *Syncer {
	accounts := []Account{{Key: me, KoboToken: "sekrit", Hardcover: hc}}
	sync := New(lib, nil, nil, s, func() []Account { return accounts }, dryRun)
	return sync
}

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
	sync := syncer(lib, s, hc, false)
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
	events := bookEvents(t, s)
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
	sync := syncer(lib, s, hc, false)
	sync.Once(context.Background())

	hc.statuses = map[int]int{} // both taken off Hardcover's read shelf
	sync = syncer(lib, s, hc, false)
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
	sync := syncer(lib, s, hc, false)
	sync.Once(ctx)
	// The Kobo reports it finished but Hardcover hasn't got the write yet.
	s.Update(ctx, me, "u-read", func(b *store.Book, _ bool) bool { b.Pending = true; return true })
	hc.statuses = map[int]int{}
	sync = syncer(lib, s, hc, false)
	sync.Once(ctx)
	if len(lib.sets) != 1 {
		t.Fatalf("sets = %v", lib.sets)
	}
}

func TestDryRunCallsNothingThenLiveActs(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	dry := syncer(lib, s, hc, true)
	dry.Once(ctx)
	dry.Once(ctx)
	if lib.reads != 0 || len(lib.sets) != 0 {
		t.Fatalf("dry run: reads %d, sets %v", lib.reads, lib.sets)
	}
	events := bookEvents(t, s)
	if len(events) != 2 || !strings.Contains(events[0].Detail, "would") {
		t.Fatalf("events = %+v", events)
	}
	live := syncer(lib, s, hc, false)
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
	sync := syncer(lib, s, hc, false)
	sync.Once(ctx)
	b, _, _ := s.Get(ctx, me, "u-unknown")
	if b.HardcoverID != 3 || b.Outcome != store.Confirmed || b.Title != "" {
		t.Fatalf("book = %+v", b)
	}
}

func bookEvents(t *testing.T, s *store.Store) []store.Event {
	t.Helper()
	all, err := s.RecentEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range all {
		if e.EntitlementID != "" {
			out = append(out, e)
		}
	}
	return out
}

func TestCollectionsFollowHardcover(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	hc.lists = []hardcover.List{{ID: 7, Name: "Dresden", BookIDs: []int{1, 3}}, {ID: 8, Name: "Read", BookIDs: []int{2}}}
	syncer(lib, s, hc, false).Once(ctx)
	if got := lib.tags["tag-Hardcover"]; strings.Join(got, ",") != "u-read,u-reading,u-unread" {
		t.Fatalf("Hardcover collection = %v", got)
	}
	if got := lib.tags["tag-Dresden"]; strings.Join(got, ",") != "u-read,u-unread" {
		t.Fatalf("Dresden = %v", got)
	}
	if _, ok := lib.tags["tag-Read (2)"]; !ok {
		t.Fatalf("a list named like a status shared its shelf: %v", lib.names)
	}

	// A second run with nothing changed sends nothing.
	lib.calls = nil
	syncer(lib, s, hc, false).Once(ctx)
	if len(lib.calls) != 0 {
		t.Fatalf("calls = %v", lib.calls)
	}

	// Moved shelves, a renamed list, a deleted list, and one deleted on the Kobo.
	hc.statuses = map[int]int{1: 3, 2: 3, 3: 3}
	hc.lists = []hardcover.List{{ID: 7, Name: "The Dresden Files", BookIDs: []int{1}}}
	delete(lib.tags, "tag-Want to Read")
	delete(lib.names, "tag-Want to Read")
	syncer(lib, s, hc, false).Once(ctx)
	if got := lib.tags["tag-Dresden"]; strings.Join(got, ",") != "u-read" || lib.names["tag-Dresden"] != "The Dresden Files" {
		t.Fatalf("renamed list = %v %q", got, lib.names["tag-Dresden"])
	}
	if _, ok := lib.tags["tag-Read (2)"]; ok {
		t.Error("deleted list's collection kept")
	}
	if got := lib.tags["tag-Read"]; len(got) != 3 {
		t.Fatalf("Read = %v", got)
	}
}

func TestOnlyUnopenedCopiesGetHardcoverProgress(t *testing.T) {
	s, lib, hc, _ := setup(t)
	ctx := context.Background()
	hc.statuses = map[int]int{1: 2, 2: 2}
	hc.progress = map[int]float64{1: 37.5, 2: 60}
	twelve := 12.0
	lib.states["u-reading"] = State{Status: statusReading, Percent: &twelve, Located: true}
	syncer(lib, s, hc, false).Once(ctx)
	if strings.Join(lib.sets, ",") != "u-read=Reading" {
		t.Fatalf("sets = %v", lib.sets)
	}
	if p := lib.percents["u-read"]; p != 37.5 {
		t.Errorf("percent = %v", p)
	}
	lib.sets = nil
	syncer(lib, s, hc, false).Once(ctx)
	if len(lib.sets) != 0 {
		t.Fatalf("unchanged progress sent again: %v", lib.sets)
	}
}
