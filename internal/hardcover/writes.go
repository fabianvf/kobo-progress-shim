package hardcover

import (
	"context"
	"fmt"
	"math"
)

// Hardcover's user_book_statuses ids.
const (
	statusReading = 2
	statusRead    = 3
)

// Want is the state the Kobo reports for a book. Reconcile compares it with
// Hardcover and sends only the difference, which is what makes repeated syncs
// safe: nothing here adds a read unless Hardcover has no read to update.
type Want struct {
	BookID  int
	ISBN    string
	Read    bool // otherwise currently reading
	Percent int
	Date    string // YYYY-MM-DD
	Rating  int    // 0 means no new rating to send
}

type edition struct {
	ID    int  `json:"id"`
	Pages *int `json:"pages"`
}

type userBookRead struct {
	ID            int     `json:"id"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
	ProgressPages *int    `json:"progress_pages"`
	EditionID     *int    `json:"edition_id"`
}

type userBook struct {
	ID       int            `json:"id"`
	StatusID int            `json:"status_id"`
	Edition  *edition       `json:"edition"`
	Reads    []userBookRead `json:"user_book_reads"`
}

// Reconcile moves Hardcover to want and describes what it did. With dryRun it
// reads Hardcover and describes what it would do, sending no mutations.
func (c *Client) Reconcile(ctx context.Context, want Want, dryRun bool) ([]string, error) {
	current, err := c.userBook(ctx, want.BookID)
	if err != nil {
		return nil, err
	}
	ed, err := c.editionByISBN(ctx, want.ISBN)
	if err != nil {
		return nil, err
	}

	var done []string
	var prior map[int]bool // reads from before this call changed the status, nil if it didn't
	if current == nil {
		status := statusReading
		if want.Read {
			status = statusRead
		}
		done = append(done, fmt.Sprintf("add to shelf as %s", statusName(status)))
		if dryRun {
			current = &userBook{StatusID: status, Edition: ed}
		} else {
			if err := c.insertUserBook(ctx, want.BookID, status, ed); err != nil {
				return done, err
			}
			// Read back rather than assume, because adding a book may create a
			// read row, and adding a second would duplicate the read.
			if current, err = c.userBook(ctx, want.BookID); err != nil {
				return done, err
			}
			if current == nil {
				return done, fmt.Errorf("hardcover: book %d missing after insert", want.BookID)
			}
			prior = map[int]bool{}
		}
	}

	if current.StatusID == statusRead && !want.Read {
		done = append(done, "already read, keeping it read")
		return c.applyRating(ctx, current, want, dryRun, done)
	}

	status := statusReading
	if want.Read {
		status = statusRead
	}
	if current.StatusID != status {
		done = append(done, fmt.Sprintf("set status to %s", statusName(status)))
		if !dryRun {
			prior = readIDs(current.Reads)
			if err := c.updateUserBook(ctx, current.ID, map[string]any{"status_id": status}); err != nil {
				return done, err
			}
			// Marking a book read makes Hardcover add a read finished today, even
			// when the book already has one.
			if current, err = c.userBook(ctx, want.BookID); err != nil {
				return done, err
			}
			if current == nil {
				return done, fmt.Errorf("hardcover: book %d missing after status change", want.BookID)
			}
			if want.Read && len(prior) > 0 {
				if err := c.removeNewReads(ctx, current, prior); err != nil {
					return done, err
				}
			}
		}
	}

	if ed == nil {
		ed = current.Edition
	}
	steps, err := c.reconcileRead(ctx, current, ed, want, prior, dryRun)
	done = append(done, steps...)
	if err != nil {
		return done, err
	}
	return c.applyRating(ctx, current, want, dryRun, done)
}

func (c *Client) applyRating(ctx context.Context, current *userBook, want Want, dryRun bool, done []string) ([]string, error) {
	if want.Rating == 0 {
		return done, nil
	}
	done = append(done, fmt.Sprintf("rate %d stars", want.Rating))
	if dryRun {
		return done, nil
	}
	return done, c.updateUserBook(ctx, current.ID, map[string]any{"rating": want.Rating})
}

// reconcileRead updates the open read, or adds one only when there isn't one.
// Every field is sent on update, as CWA does, so none gets cleared by omission.
func (c *Client) reconcileRead(ctx context.Context, current *userBook, ed *edition, want Want, prior map[int]bool, dryRun bool) ([]string, error) {
	var open *userBookRead
	for i := range current.Reads {
		if current.Reads[i].FinishedAt == nil {
			open = &current.Reads[i]
			break
		}
	}
	if open == nil && want.Read && len(current.Reads) > 0 {
		if prior == nil || len(prior) > 0 {
			return nil, nil // already has a finished read, and re-reads aren't tracked
		}
		open = &current.Reads[0] // Hardcover just created it, dated today rather than want.Date
	}

	var pages *int
	if ed != nil && ed.Pages != nil && *ed.Pages > 0 {
		p := int(math.Round(float64(*ed.Pages) * float64(want.Percent) / 100))
		pages = &p
	}

	object := map[string]any{}
	if pages != nil {
		object["progress_pages"] = *pages
	}
	if ed != nil {
		object["edition_id"] = ed.ID
	}
	if want.Read {
		object["finished_at"] = want.Date
	}

	if open == nil {
		object["started_at"] = want.Date
		description := describeRead("add a read", pages, ed, want)
		if dryRun {
			return []string{description}, nil
		}
		return []string{description}, c.mutateRead(ctx, `mutation($id: Int!, $o: DatesReadInput!) {
			result: insert_user_book_read(user_book_id: $id, user_book_read: $o) { id error }
		}`, current.ID, object)
	}

	sameProgress := pages == nil || (open.ProgressPages != nil && *open.ProgressPages == *pages)
	if sameProgress && !want.Read {
		return nil, nil
	}
	object["started_at"] = open.StartedAt
	if _, ok := object["edition_id"]; !ok && open.EditionID != nil {
		object["edition_id"] = *open.EditionID
	}
	if _, ok := object["progress_pages"]; !ok && open.ProgressPages != nil {
		object["progress_pages"] = *open.ProgressPages
	}
	description := describeRead("update the read", pages, ed, want)
	if dryRun {
		return []string{description}, nil
	}
	return []string{description}, c.mutateRead(ctx, `mutation($id: Int!, $o: DatesReadInput!) {
		result: update_user_book_read(id: $id, object: $o) { id error }
	}`, open.ID, object)
}

// removeNewReads deletes reads that aren't in prior and drops them from current.
func (c *Client) removeNewReads(ctx context.Context, current *userBook, prior map[int]bool) error {
	var kept []userBookRead
	for _, read := range current.Reads {
		if prior[read.ID] {
			kept = append(kept, read)
			continue
		}
		if err := c.mutate(ctx, `mutation($id: Int!) { result: delete_user_book_read(id: $id) { id error } }`,
			map[string]any{"id": read.ID}); err != nil {
			return err
		}
	}
	current.Reads = kept
	return nil
}

func readIDs(reads []userBookRead) map[int]bool {
	ids := make(map[int]bool, len(reads))
	for _, read := range reads {
		ids[read.ID] = true
	}
	return ids
}

func describeRead(action string, pages *int, ed *edition, want Want) string {
	s := action
	if pages != nil {
		s += fmt.Sprintf(" to page %d of %d (%d%%)", *pages, *ed.Pages, want.Percent)
	} else {
		s += " with no progress, edition has no page count"
	}
	if want.Read {
		s += ", finished " + want.Date
	}
	return s
}

func statusName(id int) string {
	if id == statusRead {
		return "read"
	}
	return "currently reading"
}

func (c *Client) userBook(ctx context.Context, bookID int) (*userBook, error) {
	const q = `query($b: Int!) {
		me { user_books(where: {book_id: {_eq: $b}}) {
			id status_id edition { id pages }
			user_book_reads(order_by: {id: desc}) { id started_at finished_at progress_pages edition_id }
		} }
	}`
	var out struct {
		Me []struct {
			UserBooks []userBook `json:"user_books"`
		} `json:"me"`
	}
	if err := c.query(ctx, q, map[string]any{"b": bookID}, &out); err != nil {
		return nil, err
	}
	if len(out.Me) == 0 || len(out.Me[0].UserBooks) == 0 {
		return nil, nil
	}
	return &out.Me[0].UserBooks[0], nil
}

func (c *Client) editionByISBN(ctx context.Context, isbn string) (*edition, error) {
	if isbn == "" {
		return nil, nil
	}
	const q = `query($i: String!) { editions(where: {isbn_13: {_eq: $i}}, limit: 1) { id pages } }`
	var out struct {
		Editions []edition `json:"editions"`
	}
	if err := c.query(ctx, q, map[string]any{"i": isbn}, &out); err != nil {
		return nil, err
	}
	if len(out.Editions) == 0 {
		return nil, nil
	}
	return &out.Editions[0], nil
}

func (c *Client) insertUserBook(ctx context.Context, bookID, status int, ed *edition) error {
	object := map[string]any{"book_id": bookID, "status_id": status}
	if ed != nil {
		object["edition_id"] = ed.ID
	}
	return c.mutate(ctx, `mutation($o: UserBookCreateInput!) { result: insert_user_book(object: $o) { id error } }`,
		map[string]any{"o": object})
}

func (c *Client) updateUserBook(ctx context.Context, id int, object map[string]any) error {
	return c.mutate(ctx, `mutation($id: Int!, $o: UserBookUpdateInput!) { result: update_user_book(id: $id, object: $o) { id error } }`,
		map[string]any{"id": id, "o": object})
}

func (c *Client) mutateRead(ctx context.Context, q string, id int, object map[string]any) error {
	return c.mutate(ctx, q, map[string]any{"id": id, "o": object})
}

// mutate also checks the error field, because Hardcover can answer 200 with
// one set and nothing done.
func (c *Client) mutate(ctx context.Context, q string, vars map[string]any) error {
	var out struct {
		Result *struct {
			Error *string `json:"error"`
		} `json:"result"`
	}
	if err := c.query(ctx, q, vars, &out); err != nil {
		return err
	}
	if out.Result == nil {
		return fmt.Errorf("hardcover: empty mutation result")
	}
	if out.Result.Error != nil && *out.Result.Error != "" {
		return fmt.Errorf("hardcover: %s", *out.Result.Error)
	}
	return nil
}
