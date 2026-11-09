package wanted

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// OpenLibrary answers 500 to an empty author, which Hardcover books with no
// author on record sent every run.
func TestSearchWithNoAuthorLeavesItOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, sent := r.URL.Query()["author"]; sent {
			http.Error(w, "empty author", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"docs":[{"key":"/works/OL1W","title":"Hope's End"}]}`))
	}))
	defer srv.Close()
	o := &OpenLibrary{base: srv.URL, http: srv.Client()}
	works, err := o.Search(context.Background(), "Hope's End", "")
	if err != nil || len(works) != 1 || works[0].ID != "OL1W" {
		t.Fatalf("works = %v, err = %v", works, err)
	}
}
