package store

import (
	"context"
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
	book := Book{EntitlementID: "e", RevisionID: "r", Title: "Mort", Authors: []string{"Terry Pratchett"}, Outcome: "review"}
	if err := s.Put(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "e", 42); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.ByRevision(ctx, "r")
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
	s.Confirm(ctx, "a", 1)
	s.Confirm(ctx, "b", 2)
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
	if _, err := s.Confirm(context.Background(), "missing", 1); err == nil {
		t.Fatal("expected an error")
	}
}
