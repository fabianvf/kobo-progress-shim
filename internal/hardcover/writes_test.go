package hardcover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHardcover is an in-memory account that answers the queries and mutations
// the client sends, so a test can check the end state as well as the calls.
type fakeHardcover struct {
	t            *testing.T
	book         *fakeBook
	pages        int
	insertAdds   bool // whether adding a book also creates a read, as it does
	statusAdds   bool // whether marking a book read adds a read finished today, as it does
	mutations    []string
	lastObject   map[string]any
	failMutation string
}

type fakeBook struct {
	Status int
	Rating any
	Reads  []map[string]any
}

func (f *fakeHardcover) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	json.Unmarshal(body, &req)
	object, _ := req.Variables["o"].(map[string]any)

	respond := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	ok := map[string]any{"result": map[string]any{"id": 1, "error": nil}}

	switch {
	case strings.Contains(req.Query, "mutation"):
		name := strings.TrimSpace(strings.SplitN(strings.SplitN(req.Query, "result:", 2)[1], "(", 2)[0])
		f.mutations = append(f.mutations, name)
		f.lastObject = object
		if name == f.failMutation {
			respond(map[string]any{"result": map[string]any{"id": nil, "error": "refused"}})
			return
		}
		switch name {
		case "insert_user_book":
			f.book = &fakeBook{Status: int(object["status_id"].(float64))}
			if f.insertAdds {
				f.book.Reads = append(f.book.Reads, map[string]any{"id": 50, "started_at": nil, "finished_at": nil, "progress_pages": nil, "edition_id": nil})
			}
		case "update_user_book":
			if s, ok := object["status_id"]; ok {
				f.book.Status = int(s.(float64))
				if f.statusAdds && f.book.Status == statusRead {
					f.book.Reads = append([]map[string]any{{"id": 90, "started_at": nil, "finished_at": "2026-12-25", "progress_pages": nil, "edition_id": nil}}, f.book.Reads...)
				}
			}
			if rating, ok := object["rating"]; ok {
				f.book.Rating = rating
			}
		case "insert_user_book_read":
			read := map[string]any{"id": 60 + len(f.book.Reads), "finished_at": nil, "started_at": nil, "progress_pages": nil, "edition_id": nil}
			for k, v := range object {
				read[k] = v
			}
			f.book.Reads = append([]map[string]any{read}, f.book.Reads...)
		case "delete_user_book_read":
			var kept []map[string]any
			for _, read := range f.book.Reads {
				if int(toFloat(read["id"])) != int(req.Variables["id"].(float64)) {
					kept = append(kept, read)
				}
			}
			f.book.Reads = kept
		case "update_user_book_read":
			for _, read := range f.book.Reads {
				if int(toFloat(read["id"])) == int(req.Variables["id"].(float64)) {
					for k, v := range object {
						read[k] = v
					}
				}
			}
		}
		respond(ok)
	case strings.Contains(req.Query, "editions"):
		respond(map[string]any{"editions": []map[string]any{{"id": 7, "pages": f.pages}}})
	default:
		books := []map[string]any{}
		if f.book != nil {
			books = append(books, map[string]any{"id": 100, "status_id": f.book.Status, "edition": nil, "user_book_reads": f.book.Reads})
		}
		respond(map[string]any{"me": []map[string]any{{"user_books": books}}})
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func client(t *testing.T, f *fakeHardcover) *Client {
	t.Helper()
	f.t = t
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	c := New("token")
	c.endpoint = server.URL
	tick := make(chan time.Time)
	close(tick)
	c.tick = tick
	return c
}

var reading = Want{BookID: 1, ISBN: "9780061801150", Percent: 50, Date: "2026-11-02"}

func TestNewBookThenRepeatedSyncsAddOneRead(t *testing.T) {
	for _, insertAdds := range []bool{false, true} {
		f := &fakeHardcover{pages: 200, insertAdds: insertAdds}
		c := client(t, f)
		for range 3 {
			if _, err := c.Reconcile(context.Background(), reading, false); err != nil {
				t.Fatal(err)
			}
		}
		if len(f.book.Reads) != 1 {
			t.Fatalf("insertAdds=%v: %d reads", insertAdds, len(f.book.Reads))
		}
		if got := toFloat(f.book.Reads[0]["progress_pages"]); got != 100 {
			t.Errorf("progress_pages = %v", got)
		}
		if f.book.Status != statusReading {
			t.Errorf("status = %d", f.book.Status)
		}
		if n := len(f.mutations); n != 2 {
			t.Errorf("insertAdds=%v: %d mutations %v, want the repeats to send none", insertAdds, n, f.mutations)
		}
	}
}

func TestFinishingClosesTheOpenRead(t *testing.T) {
	f := &fakeHardcover{pages: 200}
	c := client(t, f)
	c.Reconcile(context.Background(), reading, false)
	finished := reading
	finished.Read, finished.Percent, finished.Date = true, 100, "2026-11-09"
	for range 2 {
		if _, err := c.Reconcile(context.Background(), finished, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.book.Reads) != 1 || f.book.Reads[0]["finished_at"] != "2026-11-09" || f.book.Status != statusRead {
		t.Fatalf("book = %+v", f.book)
	}
	if f.book.Reads[0]["started_at"] != "2026-11-02" {
		t.Errorf("started_at lost: %v", f.book.Reads[0]["started_at"])
	}
}

func TestReadStaysReadOnHardcover(t *testing.T) {
	finishedAt := "2024-09-27"
	f := &fakeHardcover{pages: 200, book: &fakeBook{Status: statusRead, Reads: []map[string]any{{"id": 1, "finished_at": finishedAt}}}}
	c := client(t, f)
	done, err := c.Reconcile(context.Background(), reading, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 0 || f.book.Status != statusRead {
		t.Fatalf("mutations %v, status %d", f.mutations, f.book.Status)
	}
	if len(done) != 1 || !strings.Contains(done[0], "keeping it read") {
		t.Errorf("done = %v", done)
	}
}

func TestFinishedOnBothAddsNothing(t *testing.T) {
	f := &fakeHardcover{pages: 200, book: &fakeBook{Status: statusRead, Reads: []map[string]any{{"id": 1, "finished_at": "2024-09-27"}}}}
	c := client(t, f)
	finished := reading
	finished.Read, finished.Percent = true, 100
	if _, err := c.Reconcile(context.Background(), finished, false); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 0 {
		t.Fatalf("mutations = %v", f.mutations)
	}
}

func TestMarkingReadKeepsAnEarlierRead(t *testing.T) {
	f := &fakeHardcover{pages: 200, statusAdds: true, book: &fakeBook{Status: 1, Reads: []map[string]any{{"id": 1, "started_at": "2026-09-04", "finished_at": "2026-09-21"}}}}
	c := client(t, f)
	finished := reading
	finished.Read, finished.Percent, finished.Date = true, 100, "2026-11-09"
	for range 2 {
		if _, err := c.Reconcile(context.Background(), finished, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.book.Reads) != 1 || f.book.Reads[0]["finished_at"] != "2026-09-21" || f.book.Status != statusRead {
		t.Fatalf("book = %+v", f.book)
	}
}

func TestMarkingReadWithNoReadsUsesTheKoboDate(t *testing.T) {
	f := &fakeHardcover{pages: 200, statusAdds: true, book: &fakeBook{Status: 1}}
	c := client(t, f)
	finished := reading
	finished.Read, finished.Percent, finished.Date = true, 100, "2026-11-09"
	for range 2 {
		if _, err := c.Reconcile(context.Background(), finished, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.book.Reads) != 1 || f.book.Reads[0]["finished_at"] != "2026-11-09" {
		t.Fatalf("book = %+v", f.book)
	}
	if got := toFloat(f.book.Reads[0]["progress_pages"]); got != 200 {
		t.Errorf("progress_pages = %v", got)
	}
}

func TestDeleteFailureIsAnError(t *testing.T) {
	f := &fakeHardcover{pages: 200, statusAdds: true, failMutation: "delete_user_book_read", book: &fakeBook{Status: 1, Reads: []map[string]any{{"id": 1, "finished_at": "2026-09-21"}}}}
	c := client(t, f)
	finished := reading
	finished.Read, finished.Percent = true, 100
	if _, err := c.Reconcile(context.Background(), finished, false); err == nil {
		t.Fatal("no error")
	}
}

func TestDryRunSendsNoMutations(t *testing.T) {
	f := &fakeHardcover{pages: 200}
	c := client(t, f)
	done, err := c.Reconcile(context.Background(), Want{BookID: 1, ISBN: reading.ISBN, Percent: 50, Date: reading.Date, Rating: 4}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 0 || f.book != nil {
		t.Fatalf("mutations = %v", f.mutations)
	}
	text := strings.Join(done, "; ")
	for _, want := range []string{"add to shelf as currently reading", "add a read to page 100 of 200", "rate 4 stars"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing %q", text, want)
		}
	}
}

func TestRatingIsSentOnlyWhenAsked(t *testing.T) {
	f := &fakeHardcover{pages: 200, book: &fakeBook{Status: statusReading, Rating: 3.5}}
	c := client(t, f)
	c.Reconcile(context.Background(), reading, false)
	if f.book.Rating != 3.5 {
		t.Fatalf("rating overwritten: %v", f.book.Rating)
	}
	rated := reading
	rated.Rating = 4
	if _, err := c.Reconcile(context.Background(), rated, false); err != nil {
		t.Fatal(err)
	}
	if toFloat(f.book.Rating) != 4 {
		t.Fatalf("rating = %v", f.book.Rating)
	}
}

func TestMutationErrorFieldIsAnError(t *testing.T) {
	f := &fakeHardcover{pages: 200, failMutation: "insert_user_book"}
	c := client(t, f)
	if _, err := c.Reconcile(context.Background(), reading, false); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoPageCountStillSetsStatus(t *testing.T) {
	f := &fakeHardcover{pages: 0}
	c := client(t, f)
	done, err := c.Reconcile(context.Background(), reading, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.book == nil || f.book.Status != statusReading {
		t.Fatalf("book = %+v", f.book)
	}
	if !strings.Contains(strings.Join(done, "; "), "no page count") {
		t.Errorf("done = %v", done)
	}
}
