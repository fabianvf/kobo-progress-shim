package activity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

func TestPageAndConfirm(t *testing.T) {
	ctx := context.Background()
	books, err := store.Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer books.Close()
	books.Put(ctx, store.Book{User: "u", EntitlementID: "e", Title: "<Whispers>", Outcome: "review", Reason: "no exact search match", Suggestion: "432534: Whispers Under Ground"})
	books.AddEvent(ctx, store.Event{Time: time.Now(), EntitlementID: "e", Title: "<Whispers>", Detail: "reading at 6%"})

	mux := http.NewServeMux()
	Register(mux, books, true)
	server := httptest.NewServer(mux)
	defer server.Close()

	body := get(t, server.URL)
	for _, want := range []string{"Needs a match (1)", "&lt;Whispers&gt;", "432534: Whispers Under Ground", "reading at 6%"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}

	resp, err := http.PostForm(server.URL+"/confirm", url.Values{"id": {"e"}, "hardcover_id": {"not a number"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status %d", resp.StatusCode)
	}

	resp, err = http.PostForm(server.URL+"/confirm", url.Values{"user": {"u"}, "id": {"e"}, "hardcover_id": {"432534"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("confirm: status %d", resp.StatusCode)
	}
	if body := get(t, server.URL); !strings.Contains(body, "Needs a match (0)") {
		t.Error("book still waiting after confirm")
	}
	if b, _, _ := books.Get(ctx, "u", "e"); !b.Resolved() || b.HardcoverID != 432534 {
		t.Errorf("book = %+v", b)
	}
}

func TestConfirmRefusesCrossSitePost(t *testing.T) {
	ctx := context.Background()
	books, err := store.Open(filepath.Join(t.TempDir(), "shim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer books.Close()
	books.Put(ctx, store.Book{EntitlementID: "e", Outcome: "review"})
	mux := http.NewServeMux()
	Register(mux, books, true)
	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/confirm", strings.NewReader("id=e&hardcover_id=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d", resp.StatusCode)
	}
	if b, _, _ := books.Get(ctx, "", "e"); b.Resolved() {
		t.Error("cross-site post saved a match")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
