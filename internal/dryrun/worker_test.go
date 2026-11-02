package dryrun

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/fabianvf/kobo-progress-shim/internal/capture"
	"github.com/fabianvf/kobo-progress-shim/internal/kobostore"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
)

const secret = "Bearer sekrit-value"

type fakeStore struct {
	meta  kobostore.Metadata
	err   error
	calls int
	auth  string
}

func (f *fakeStore) Metadata(_ context.Context, _, authorization, _ string) (kobostore.Metadata, error) {
	f.calls++
	f.auth = authorization
	return f.meta, f.err
}

type fakeLookup struct{ books []match.Book }

func (f fakeLookup) BooksByISBN(context.Context, string) ([]match.Book, error) { return f.books, nil }
func (f fakeLookup) Search(context.Context, string, string) ([]match.Book, error) {
	return f.books, nil
}

func progress() capture.Progress {
	return capture.Progress{EntitlementID: "id", Percent: 6, Status: "Reading", Authorization: secret, Source: "OEBPS/9780061801150_x.xhtml"}
}

func capturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return &buf
}

func TestMatchesAndNeverLogsAuth(t *testing.T) {
	logged := capturedLog(t)
	store := &fakeStore{meta: kobostore.Metadata{Title: "The Light Fantastic", Contributors: []string{"Terry Pratchett"}, ISBN: "9780061801150"}}
	w := New(store, fakeLookup{books: []match.Book{{ID: 427416, Title: "The Light Fantastic", Authors: []string{"Terry Pratchett"}}}})
	w.Submit(progress())
	w.Submit(progress())
	w.Close()

	out := logged.String()
	if store.auth != secret {
		t.Errorf("store got wrong auth")
	}
	if store.calls != 1 {
		t.Errorf("store called %d times, want a remembered answer", store.calls)
	}
	if strings.Contains(out, "sekrit") {
		t.Fatalf("auth logged: %s", out)
	}
	for _, want := range []string{`"hardcover_book_id":427416`, `"outcome":"automatic"`, `"isbn_from":"store"`, `"percent":6`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestFailedLookupIsRetriedAndFallsBackToBookmark(t *testing.T) {
	logged := capturedLog(t)
	store := &fakeStore{err: errors.New("status 401")}
	w := New(store, nil)
	w.Submit(progress())
	w.Submit(progress())
	w.Close()

	if store.calls != 2 {
		t.Errorf("store called %d times, want a retry", store.calls)
	}
	if out := logged.String(); !strings.Contains(out, `"isbn_from":"bookmark"`) || !strings.Contains(out, "status 401") {
		t.Errorf("log = %s", out)
	}
}

func TestNotFoundIsRemembered(t *testing.T) {
	capturedLog(t)
	store := &fakeStore{err: kobostore.ErrNotFound}
	w := New(store, nil)
	w.Submit(progress())
	w.Submit(progress())
	w.Close()
	if store.calls != 1 {
		t.Errorf("store called %d times", store.calls)
	}
}
