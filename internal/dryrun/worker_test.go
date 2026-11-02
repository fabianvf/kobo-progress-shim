package dryrun

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

const secret = "Bearer sekrit-value"

type fakeKobo struct {
	meta  kobostore.Metadata
	err   error
	calls int
	auth  string
}

func (f *fakeKobo) Metadata(_ context.Context, _ string, device http.Header) (kobostore.Metadata, error) {
	f.calls++
	f.auth = device.Get("Authorization")
	return f.meta, f.err
}

type fakeLookup struct{ books []match.Book }

func (f fakeLookup) BooksByISBN(context.Context, string) ([]match.Book, error) { return f.books, nil }
func (f fakeLookup) Search(context.Context, string, string) ([]match.Book, error) {
	return f.books, nil
}

var lightFantastic = match.Book{ID: 427416, Title: "The Light Fantastic", Authors: []string{"Terry Pratchett"}}

func found() *fakeKobo {
	return &fakeKobo{meta: kobostore.Metadata{Title: "The Light Fantastic", Contributors: []string{"Terry Pratchett"}, ISBN: "9780061801150", RevisionID: "rev"}}
}

func progress(percent int, status string) capture.Progress {
	return capture.Progress{EntitlementID: "id", Percent: percent, Status: status, Headers: http.Header{"Authorization": {secret}}, Source: "OEBPS/9780061801150_x.xhtml"}
}

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shim.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func capturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func book(t *testing.T, s *store.Store, id string) store.Book {
	t.Helper()
	b, ok, err := s.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("book %s: ok=%v err=%v", id, ok, err)
	}
	return b
}

func TestMatchesAndNeverKeepsAuth(t *testing.T) {
	logged := capturedLog(t)
	s, path := openStore(t)
	kobo := found()
	w := New(kobo, fakeLookup{books: []match.Book{lightFantastic}}, nil, s, true)
	w.Progress(progress(6, "Reading"))
	w.Progress(progress(7, "Reading"))
	w.Close()

	if kobo.auth != secret {
		t.Error("store lookup got the wrong auth")
	}
	if kobo.calls != 1 {
		t.Errorf("store called %d times, want a saved answer", kobo.calls)
	}
	b := book(t, s, "id")
	if !b.Resolved() || b.HardcoverID != 427416 || b.Percent != 7 || b.Shelf != shelfReading {
		t.Fatalf("book = %+v", b)
	}
	if !strings.Contains(logged.String(), "would set reading at 7% on Hardcover book 427416") {
		t.Errorf("log = %s", logged)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "sekrit") || bytes.Contains(data, []byte("sekrit")) {
		t.Fatal("auth was logged or stored")
	}
}

func TestFailedLookupIsRetried(t *testing.T) {
	capturedLog(t)
	s, _ := openStore(t)
	kobo := &fakeKobo{err: errors.New("status 403")}
	w := New(kobo, fakeLookup{books: []match.Book{lightFantastic}}, nil, s, true)
	w.Progress(progress(6, "Reading"))
	w.Progress(progress(7, "Reading"))
	w.Close()
	if kobo.calls != 2 {
		t.Errorf("store called %d times, want a retry", kobo.calls)
	}
	if b := book(t, s, "id"); b.Outcome != "" || b.ISBNFrom != "bookmark" {
		t.Errorf("book = %+v", b)
	}
}

func TestCWABookMatchesFromSync(t *testing.T) {
	capturedLog(t)
	s, _ := openStore(t)
	kobo := &fakeKobo{err: kobostore.ErrNotFound}
	hamilton := match.Book{ID: 545372, Title: "The Monster-God of Mamurth", Authors: []string{"Edmond Hamilton"}}
	w := New(kobo, fakeLookup{books: []match.Book{hamilton}}, nil, s, true)
	w.SyncedBook(capture.SyncedBook{EntitlementID: "cwa", RevisionID: "cwa", Title: "The monster-god of Mamurth", Authors: []string{"Edmond Hamilton"}})
	w.Progress(capture.Progress{EntitlementID: "cwa", Percent: 100, Status: "Finished"})
	w.Rating(capture.Rating{ProductID: "cwa", Stars: 4})
	w.Close()
	b := book(t, s, "cwa")
	if !b.Resolved() || b.HardcoverID != 545372 || b.Shelf != shelfRead || b.Rating != 4 {
		t.Fatalf("book = %+v", b)
	}
}

func TestReadStaysRead(t *testing.T) {
	capturedLog(t)
	s, _ := openStore(t)
	w := New(found(), fakeLookup{books: []match.Book{lightFantastic}}, nil, s, true)
	w.Progress(progress(100, "Finished"))
	w.Progress(progress(36, "Reading"))
	w.Close()
	if b := book(t, s, "id"); b.Shelf != shelfRead || b.Percent != 36 {
		t.Fatalf("book = %+v", b)
	}
}

func TestRatingFindsBookByRevision(t *testing.T) {
	logged := capturedLog(t)
	s, _ := openStore(t)
	w := New(found(), fakeLookup{books: []match.Book{lightFantastic}}, nil, s, true)
	w.Progress(progress(6, "Reading"))
	w.Rating(capture.Rating{ProductID: "rev", Stars: 5})
	w.Rating(capture.Rating{ProductID: "unknown", Stars: 3})
	w.Close()
	if b := book(t, s, "id"); b.Rating != 5 {
		t.Fatalf("book = %+v", b)
	}
	if !strings.Contains(logged.String(), "would rate 5 stars") {
		t.Errorf("log = %s", logged)
	}
	events, err := s.RecentEvents(context.Background(), 1)
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Detail, "unknown product") {
		t.Fatalf("events = %+v, err %v", events, err)
	}
}

func TestConfirmedMatchSurvivesSync(t *testing.T) {
	capturedLog(t)
	s, _ := openStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, store.Book{EntitlementID: "id", Title: "Mine", Outcome: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "id", 9); err != nil {
		t.Fatal(err)
	}
	w := New(found(), fakeLookup{}, nil, s, true)
	w.SyncedBook(capture.SyncedBook{EntitlementID: "id", Title: "Other"})
	w.Close()
	if b := book(t, s, "id"); b.HardcoverID != 9 || b.Outcome != store.Confirmed || b.Title != "Mine" {
		t.Fatalf("book = %+v", b)
	}
}

type fakeWriter struct {
	wants []hardcover.Want
	dry   []bool
	err   error
}

func (f *fakeWriter) Reconcile(_ context.Context, want hardcover.Want, dryRun bool) ([]string, error) {
	f.wants = append(f.wants, want)
	f.dry = append(f.dry, dryRun)
	return []string{"set status"}, f.err
}

func TestWritesResolvedBooksOnly(t *testing.T) {
	logged := capturedLog(t)
	s, _ := openStore(t)
	writer := &fakeWriter{}
	w := New(found(), fakeLookup{books: []match.Book{lightFantastic}}, writer, s, true)
	p := progress(100, "Finished")
	p.LastModified = "2026-11-09T01:02:03Z"
	w.Progress(p)
	w.Rating(capture.Rating{ProductID: "rev", Stars: 4})
	w.Close()
	if len(writer.wants) != 2 || !writer.dry[0] {
		t.Fatalf("wants = %+v, dry = %v", writer.wants, writer.dry)
	}
	got := writer.wants[0]
	if got.BookID != 427416 || !got.Read || got.Date != "2026-11-09" || got.ISBN != "9780061801150" || got.Rating != 0 {
		t.Errorf("want = %+v", got)
	}
	if writer.wants[1].Rating != 4 {
		t.Errorf("rating want = %+v", writer.wants[1])
	}
	if !strings.Contains(logged.String(), "would set status") {
		t.Errorf("log = %s", logged)
	}
	if b := book(t, s, "id"); b.RatingWritten != 0 {
		t.Errorf("dry run marked the rating written: %+v", b)
	}

	writer = &fakeWriter{}
	review := New(found(), fakeLookup{}, writer, s, true)
	review.Progress(capture.Progress{EntitlementID: "other", Percent: 50, Status: "Reading"})
	review.Close()
	if len(writer.wants) != 0 {
		t.Errorf("unmatched book written: %+v", writer.wants)
	}
}

func TestFailedWriteStaysPendingAndRetries(t *testing.T) {
	capturedLog(t)
	s, _ := openStore(t)
	writer := &fakeWriter{err: errors.New("hardcover down")}
	w := New(found(), fakeLookup{books: []match.Book{lightFantastic}}, writer, s, false)
	w.Progress(progress(6, "Reading"))
	w.Rating(capture.Rating{ProductID: "rev", Stars: 5})
	w.Close()
	b := book(t, s, "id")
	if !b.Pending || b.RatingWritten != 0 {
		t.Fatalf("book = %+v", b)
	}

	writer.err = nil
	ctx := context.Background()
	retry := New(found(), fakeLookup{}, writer, s, false)
	if err := retry.handleRetry(ctx); err != nil {
		t.Fatal(err)
	}
	retry.Close()
	b = book(t, s, "id")
	if b.Pending || b.RatingWritten != 5 {
		t.Fatalf("after retry: %+v", b)
	}
	if last := writer.wants[len(writer.wants)-1]; last.Rating != 5 {
		t.Errorf("retry want = %+v", last)
	}
}
