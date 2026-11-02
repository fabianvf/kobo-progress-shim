// Package store keeps what the shim has worked out about each book, so a pod
// restart doesn't lose matches or decisions made by hand.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// Outcomes beyond match.Automatic and match.Review.
const Confirmed = "confirmed"

type Book struct {
	EntitlementID string
	RevisionID    string
	Title         string
	Authors       []string
	ISBN          string
	ISBNFrom      string
	Lookup        string
	Outcome       string
	HardcoverID   int
	Reason        string
	Suggestion    string
	Percent       int
	Status        string
	Shelf         string
	Rating        int
	RatingWritten int
	Date          string // when the Kobo last reported, YYYY-MM-DD
	Pending       bool   // Hardcover hasn't got the latest state yet
}

// Resolved reports whether the book has a Hardcover id that can be written to.
func (b Book) Resolved() bool {
	return b.HardcoverID != 0 && (b.Outcome == "automatic" || b.Outcome == Confirmed)
}

type Event struct {
	Time          time.Time
	EntitlementID string
	Title         string
	Detail        string
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS books (
	entitlement_id TEXT PRIMARY KEY,
	revision_id TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL,
	outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS books_revision ON books (revision_id);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	time TEXT NOT NULL,
	entitlement_id TEXT NOT NULL,
	title TEXT NOT NULL,
	detail TEXT NOT NULL
);`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the worker and the activity page share one writer
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Get returns false when the book isn't known yet.
func (s *Store) Get(ctx context.Context, id string) (Book, bool, error) {
	return s.one(ctx, `SELECT data FROM books WHERE entitlement_id = ?`, id)
}

// ByRevision finds a book from a product id. Rating requests use that id rather
// than the entitlement id, and on store and Libby books the two differ.
func (s *Store) ByRevision(ctx context.Context, id string) (Book, bool, error) {
	return s.one(ctx, `SELECT data FROM books WHERE revision_id = ? OR entitlement_id = ? LIMIT 1`, id, id)
}

func (s *Store) one(ctx context.Context, query string, args ...any) (Book, bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Book{}, false, nil
	}
	if err != nil {
		return Book{}, false, err
	}
	var b Book
	return b, true, json.Unmarshal([]byte(data), &b)
}

func (s *Store) Put(ctx context.Context, b Book) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO books (entitlement_id, revision_id, data, outcome) VALUES (?, ?, ?, ?)
		ON CONFLICT (entitlement_id) DO UPDATE SET
			revision_id = excluded.revision_id, data = excluded.data, outcome = excluded.outcome`,
		b.EntitlementID, b.RevisionID, string(data), b.Outcome)
	return err
}

// NeedingReview returns books waiting for a person to choose a match.
func (s *Store) NeedingReview(ctx context.Context) ([]Book, error) {
	return s.list(ctx, `SELECT data FROM books WHERE outcome = 'review' ORDER BY entitlement_id`)
}

func (s *Store) list(ctx context.Context, query string) ([]Book, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var books []Book
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var b Book
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

// Confirm records a match chosen by hand. It replaces any automatic match too,
// so a wrong one can be corrected.
func (s *Store) Confirm(ctx context.Context, id string, hardcoverID int) (Book, error) {
	b, ok, err := s.Get(ctx, id)
	if err != nil {
		return Book{}, err
	}
	if !ok {
		return Book{}, errors.New("store: unknown book")
	}
	b.Outcome, b.HardcoverID, b.Reason = Confirmed, hardcoverID, "chosen by hand"
	b.Pending = b.Shelf != "" // replay the latest progress on the chosen book
	return b, s.Put(ctx, b)
}

// PendingBooks returns resolved books whose latest state hasn't reached Hardcover.
func (s *Store) PendingBooks(ctx context.Context) ([]Book, error) {
	return s.list(ctx, `SELECT data FROM books WHERE json_extract(data, '$.Pending') = 1 ORDER BY entitlement_id`)
}

func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (time, entitlement_id, title, detail) VALUES (?, ?, ?, ?)`,
		e.Time.UTC().Format(time.RFC3339), e.EntitlementID, e.Title, e.Detail)
	return err
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT time, entitlement_id, title, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var when string
		if err := rows.Scan(&when, &e.EntitlementID, &e.Title, &e.Detail); err != nil {
			return nil, err
		}
		if e.Time, err = time.Parse(time.RFC3339, when); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
