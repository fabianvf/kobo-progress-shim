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
	User          string // accounts.Key, since CWA books have the same id for every user
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
	User          string
	EntitlementID string
	Title         string
	Detail        string
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS books (
	user TEXT NOT NULL DEFAULT '',
	entitlement_id TEXT NOT NULL,
	revision_id TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL,
	outcome TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user, entitlement_id)
);
CREATE INDEX IF NOT EXISTS books_revision ON books (user, revision_id);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	time TEXT NOT NULL,
	entitlement_id TEXT NOT NULL,
	title TEXT NOT NULL,
	detail TEXT NOT NULL,
	user TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS accounts (
	email TEXT PRIMARY KEY,
	sealed BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS requests (
	hardcover_id INTEGER PRIMARY KEY,
	data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS collections (
	user TEXT NOT NULL,
	key TEXT NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (user, key)
);
CREATE TABLE IF NOT EXISTS cwa_state (
	user TEXT NOT NULL,
	entitlement_id TEXT NOT NULL,
	status TEXT NOT NULL,
	marked INTEGER NOT NULL,
	PRIMARY KEY (user, entitlement_id)
);`

// Before accounts, books were keyed by entitlement id alone. Their rows move
// across with no user, and AdoptUnowned gives them one.
const migrateV1 = `
ALTER TABLE books RENAME TO books_v1;
DROP INDEX IF EXISTS books_revision;
` + schema + `
INSERT INTO books (user, entitlement_id, revision_id, data, outcome)
	SELECT '', entitlement_id, revision_id, data, outcome FROM books_v1;
DROP TABLE books_v1;`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the worker and the activity page share one writer
	if err := upgrade(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func upgrade(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		if !hasTable(db, "books") || hasColumn(db, "books", "user") {
			return err
		}
	}
	if !hasColumn(db, "events", "user") {
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN user TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if hasColumn(db, "books", "user") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(migrateV1); err != nil {
		return err
	}
	return tx.Commit()
}

func hasTable(db *sql.DB, table string) bool {
	var n int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return n > 0
}

func hasColumn(db *sql.DB, table, column string) bool {
	var n int
	db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0
}

// AdoptUnowned gives rows saved before accounts existed to the one account there
// is. With several accounts nobody can say whose they were, so they stay put.
func (s *Store) AdoptUnowned(ctx context.Context, user string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE books SET user = ?, data = json_set(data, '$.User', ?) WHERE user = ''`, user, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE collections SET user = ? WHERE user = ''`, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE cwa_state SET user = ? WHERE user = ''`, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET user = ? WHERE user = ''`, user); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// Get returns false when the book isn't known yet.
func (s *Store) Get(ctx context.Context, user, id string) (Book, bool, error) {
	return s.one(ctx, `SELECT data FROM books WHERE user = ? AND entitlement_id = ?`, user, id)
}

// ByRevision finds a book from a product id. Rating requests use that id rather
// than the entitlement id, and on store and Libby books the two differ.
func (s *Store) ByRevision(ctx context.Context, user, id string) (Book, bool, error) {
	return s.one(ctx, `SELECT data FROM books WHERE user = ? AND (revision_id = ? OR entitlement_id = ?) LIMIT 1`, user, id, id)
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
		INSERT INTO books (user, entitlement_id, revision_id, data, outcome) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user, entitlement_id) DO UPDATE SET
			revision_id = excluded.revision_id, data = excluded.data, outcome = excluded.outcome`,
		b.User, b.EntitlementID, b.RevisionID, string(data), b.Outcome)
	return err
}

// Update changes one book inside a transaction, so a change can't overwrite one
// the worker saved in between. fn gets a zero Book with ok false for a new one,
// and returns whether to save.
func (s *Store) Update(ctx context.Context, user, id string, fn func(b *Book, ok bool) bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data string
	var b Book
	err = tx.QueryRowContext(ctx, `SELECT data FROM books WHERE user = ? AND entitlement_id = ?`, user, id).Scan(&data)
	ok := err == nil
	switch {
	case ok:
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if !fn(&b, ok) {
		return nil
	}
	b.User, b.EntitlementID = user, id
	saved, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO books (user, entitlement_id, revision_id, data, outcome) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user, entitlement_id) DO UPDATE SET
			revision_id = excluded.revision_id, data = excluded.data, outcome = excluded.outcome`,
		b.User, b.EntitlementID, b.RevisionID, string(saved), b.Outcome); err != nil {
		return err
	}
	return tx.Commit()
}

// CWAState is what the CWA sync last knew of a book's reading status in CWA, and
// whether the sync itself set it to Finished. It's kept apart from Book so the
// worker, which saves whole books, can't overwrite it.
type CWAState struct {
	Status string
	Marked bool
}

func (s *Store) CWAState(ctx context.Context, user, id string) (CWAState, error) {
	var st CWAState
	err := s.db.QueryRowContext(ctx, `SELECT status, marked FROM cwa_state WHERE user = ? AND entitlement_id = ?`, user, id).Scan(&st.Status, &st.Marked)
	if errors.Is(err, sql.ErrNoRows) {
		return CWAState{}, nil
	}
	return st, err
}

func (s *Store) SetCWAState(ctx context.Context, user, id string, st CWAState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cwa_state (user, entitlement_id, status, marked) VALUES (?, ?, ?, ?)
		ON CONFLICT (user, entitlement_id) DO UPDATE SET status = excluded.status, marked = excluded.marked`,
		user, id, st.Status, st.Marked)
	return err
}

// Request is a Hardcover book someone wants that the library doesn't have, and
// what was done to get it. There's one per book, whoever asked for it first.
type Request struct {
	HardcoverID int
	Title       string
	Users       []string
	WorkID      string // the OpenLibrary work, when that's how it was found
	LLID        string // the id LazyLibrarian has it under, once queued or found
	// TriedHardcover is set once LazyLibrarian's Hardcover source was tried,
	// so a book it couldn't add isn't tried that way again.
	TriedHardcover bool
	State          string
	Detail         string
	Updated        time.Time
}

func (s *Store) Request(ctx context.Context, hardcoverID int) (Request, bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM requests WHERE hardcover_id = ?`, hardcoverID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}
	var r Request
	return r, true, json.Unmarshal([]byte(data), &r)
}

func (s *Store) PutRequest(ctx context.Context, r Request) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO requests (hardcover_id, data) VALUES (?, ?)
		ON CONFLICT (hardcover_id) DO UPDATE SET data = excluded.data`, r.HardcoverID, string(data))
	return err
}

func (s *Store) Requests(ctx context.Context) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM requests ORDER BY hardcover_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r Request
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Collection is a Kobo collection the CWA sync made for a user, mirroring one
// Hardcover shelf or list. Items are what the sync last put in it, as CWA book
// uuids, so it only ever changes what it added.
type Collection struct {
	Key    string // "status:1" or "list:<id>"
	Name   string
	TagID  string // CWA's shelf uuid
	Items  []string
	Synced bool // the shelf is marked to sync to the Kobo
}

func (s *Store) Collections(ctx context.Context, user string) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM collections WHERE user = ? ORDER BY key`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var c Collection
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PutCollection(ctx context.Context, user string, c Collection) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO collections (user, key, data) VALUES (?, ?, ?)
		ON CONFLICT (user, key) DO UPDATE SET data = excluded.data`, user, c.Key, string(data))
	return err
}

func (s *Store) DeleteCollection(ctx context.Context, user, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM collections WHERE user = ? AND key = ?`, user, key)
	return err
}

// SealedAccounts returns each account saved through setup, still encrypted.
// The store never sees the key, so a copy of shim.db alone exposes no tokens.
func (s *Store) SealedAccounts(ctx context.Context) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, sealed FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var email string
		var sealed []byte
		if err := rows.Scan(&email, &sealed); err != nil {
			return nil, err
		}
		out[email] = sealed
	}
	return out, rows.Err()
}

func (s *Store) PutSealedAccount(ctx context.Context, email string, sealed []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts (email, sealed) VALUES (?, ?)
		ON CONFLICT (email) DO UPDATE SET sealed = excluded.sealed`, email, sealed)
	return err
}

// LastEvent returns when anything was last recorded for a user, zero if never.
func (s *Store) LastEvent(ctx context.Context, user string) (time.Time, error) {
	var when sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT max(time) FROM events WHERE user = ?`, user).Scan(&when); err != nil || !when.Valid {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, when.String)
}

// NeedingReview returns books waiting for a person to choose a match.
func (s *Store) NeedingReview(ctx context.Context) ([]Book, error) {
	return s.list(ctx, `SELECT data FROM books WHERE outcome = 'review' ORDER BY user, entitlement_id`)
}

// UserBooks returns every book saved for one user.
func (s *Store) UserBooks(ctx context.Context, user string) ([]Book, error) {
	return s.list(ctx, `SELECT data FROM books WHERE user = ? ORDER BY entitlement_id`, user)
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]Book, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
func (s *Store) Confirm(ctx context.Context, user, id string, hardcoverID int) (Book, error) {
	b, ok, err := s.Get(ctx, user, id)
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
	return s.list(ctx, `SELECT data FROM books WHERE json_extract(data, '$.Pending') = 1 ORDER BY user, entitlement_id`)
}

func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (time, user, entitlement_id, title, detail) VALUES (?, ?, ?, ?, ?)`,
		e.Time.UTC().Format(time.RFC3339), e.User, e.EntitlementID, e.Title, e.Detail)
	return err
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT time, user, entitlement_id, title, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var when string
		if err := rows.Scan(&when, &e.User, &e.EntitlementID, &e.Title, &e.Detail); err != nil {
			return nil, err
		}
		if e.Time, err = time.Parse(time.RFC3339, when); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
