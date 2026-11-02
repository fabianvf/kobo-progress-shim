package kobostore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const id = "b3c8d491-a9bb-46b3-b6dd-3230f751e971"

func device(auth string) http.Header {
	return http.Header{"Authorization": {auth}, "User-Agent": {"kobo"}, "X-Kobo-Deviceid": {"d"}}
}

func TestMetadata(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/library/"+id+"/metadata" || r.Header.Get("Authorization") != "Bearer tok" ||
			r.Header.Get("User-Agent") != "kobo" || r.Header.Get("X-Kobo-Deviceid") != "d" {
			t.Errorf("request = %s %v", r.URL.Path, r.Header)
		}
		w.Write([]byte(`[{"Title":"The Light Fantastic","Contributors":["Terry Pratchett"],"Isbn":"9780061801150"}]`))
	}))
	defer store.Close()
	c := New()
	c.baseURL = store.URL
	got, err := c.Metadata(context.Background(), id, device("Bearer tok"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "The Light Fantastic" || got.ISBN != "9780061801150" || got.Contributors[0] != "Terry Pratchett" {
		t.Fatalf("got %+v", got)
	}
}

func TestMetadataErrors(t *testing.T) {
	status := http.StatusNotFound
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(status)
		w.Write([]byte(`{"ResponseStatus":{"ErrorCode":"Denied","Message":"nope"},"Other":"unexpected"}`))
	}))
	defer store.Close()
	c := New()
	c.baseURL = store.URL

	if _, err := c.Metadata(context.Background(), id, device("Bearer sekrit")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusForbidden
	_, err := c.Metadata(context.Background(), id, device("Bearer sekrit"))
	if err == nil || strings.Contains(err.Error(), "sekrit") || strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("403: %v", err)
	}
	for _, want := range []string{"403", "cloudflare", "Denied", "nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("403 error %q is missing %q", err, want)
		}
	}
	if _, err := c.Metadata(context.Background(), "../../user/profile", device("Bearer sekrit")); err == nil {
		t.Fatal("invalid id was requested")
	}
}
