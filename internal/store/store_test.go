package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestBooksSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shim.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	book := Book{User: "u", EntitlementID: "e", RevisionID: "r", Title: "Mort", Authors: []string{"Terry Pratchett"}, Outcome: "review"}
	if err := s.Put(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "u", "e", 42); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.ByRevision(ctx, "u", "r")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !got.Resolved() || got.HardcoverID != 42 || got.Authors[0] != "Terry Pratchett" {
		t.Fatalf("got %+v", got)
	}
}

func TestNeedingReviewAndEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Put(ctx, Book{EntitlementID: "a", Outcome: "review"})
	s.Put(ctx, Book{EntitlementID: "b", Outcome: "automatic", HardcoverID: 1})
	review, err := s.NeedingReview(ctx)
	if err != nil || len(review) != 1 || review[0].EntitlementID != "a" {
		t.Fatalf("review = %+v, err %v", review, err)
	}
	s.AddEvent(ctx, Event{Time: time.Now(), EntitlementID: "a", Detail: "first"})
	s.AddEvent(ctx, Event{Time: time.Now(), EntitlementID: "a", Detail: "second"})
	events, err := s.RecentEvents(ctx, 1)
	if err != nil || len(events) != 1 || events[0].Detail != "second" {
		t.Fatalf("events = %+v, err %v", events, err)
	}
}

func TestConfirmReplaysLatestProgress(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Put(ctx, Book{EntitlementID: "a", Outcome: "review", Shelf: "reading"})
	s.Put(ctx, Book{EntitlementID: "b", Outcome: "review"})
	s.Confirm(ctx, "", "a", 1)
	s.Confirm(ctx, "", "b", 2)
	pending, err := s.PendingBooks(ctx)
	if err != nil || len(pending) != 1 || pending[0].EntitlementID != "a" {
		t.Fatalf("pending = %+v, err %v", pending, err)
	}
}

func TestConfirmUnknownBook(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Confirm(context.Background(), "", "missing", 1); err == nil {
		t.Fatal("expected an error")
	}
}

func TestBooksAreSeparatePerUser(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Put(ctx, Book{User: "a", EntitlementID: "cwa-book", HardcoverID: 1, Outcome: "automatic"})
	s.Put(ctx, Book{User: "b", EntitlementID: "cwa-book", Outcome: "review"})
	if b, _, _ := s.Get(ctx, "a", "cwa-book"); b.HardcoverID != 1 {
		t.Fatalf("a's book = %+v", b)
	}
	if b, _, _ := s.Get(ctx, "b", "cwa-book"); b.HardcoverID != 0 || b.Outcome != "review" {
		t.Fatalf("b's book = %+v", b)
	}
	if _, ok, _ := s.ByRevision(ctx, "c", "cwa-book"); ok {
		t.Error("found another user's book")
	}
}

// The live store before accounts: books keyed by entitlement id alone.
const v1Schema = `
CREATE TABLE books (entitlement_id TEXT PRIMARY KEY, revision_id TEXT NOT NULL DEFAULT '', data TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '');
CREATE INDEX books_revision ON books (revision_id);
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, entitlement_id TEXT NOT NULL, title TEXT NOT NULL, detail TEXT NOT NULL);
INSERT INTO books VALUES ('e', 'r', '{"EntitlementID":"e","RevisionID":"r","Title":"The Light Fantastic","Outcome":"automatic","HardcoverID":427416}', 'automatic');
INSERT INTO events (time, entitlement_id, title, detail) VALUES ('2026-11-03T04:54:00Z', 'e', 'The Light Fantastic', 'update the read');`

func TestUpgradeKeepsV1Rows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shim.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(v1Schema); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok, err := s.Get(ctx, "", "e"); err != nil || !ok || b.HardcoverID != 427416 {
		t.Fatalf("migrated book = %+v, ok %v, err %v", b, ok, err)
	}
	if err := s.AdoptUnowned(ctx, "me"); err != nil {
		t.Fatal(err)
	}
	b, ok, err := s.ByRevision(ctx, "me", "r")
	if err != nil || !ok || b.User != "me" || !b.Resolved() {
		t.Fatalf("adopted book = %+v, ok %v, err %v", b, ok, err)
	}
	events, err := s.RecentEvents(ctx, 5)
	if err != nil || len(events) != 1 || events[0].User != "me" {
		t.Fatalf("events = %+v, err %v", events, err)
	}
	s.Close()

	// A second open of the upgraded file is a no-op.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if books, err := s.UserBooks(ctx, "me"); err != nil || len(books) != 1 {
		t.Fatalf("books = %+v, err %v", books, err)
	}
}
