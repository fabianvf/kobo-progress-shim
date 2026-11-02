package capture

import (
	"net/http"
	"testing"
)

const stateURI = "/kobo/tok/v1/library/b3c8d491-a9bb-46b3-b6dd-3230f751e971/state"

const stateBody = `{"ReadingStates":[{
	"CurrentBookmark":{"ProgressPercent":6,"Location":{"Source":"OEBPS/text/9780061801150_Bodymatter.xhtml"}},
	"EntitlementId":"b3c8d491-a9bb-46b3-b6dd-3230f751e971",
	"StatusInfo":{"LastModified":"2026-11-02T15:23:39Z","Status":"Reading"}}]}`

func stateExchange(method, uri string, status int, body string) *Exchange {
	request := &cappedBuffer{limit: maxCapturedBody}
	request.Write([]byte(body))
	return &Exchange{
		Method:        method,
		RequestURI:    uri,
		Status:        status,
		RequestHeader: http.Header{"Authorization": {"Bearer " + secret}, "User-Agent": {"kobo"}, "X-Kobo-Deviceid": {"d"}, "X-Forwarded-For": {"1.2.3.4"}, "Content-Length": {"9"}, "Cookie": {"session=cwa"}, "Accept-Encoding": {"gzip"}, "X-New-Header": {"kept"}},
		RequestBody:   request,
		ResponseBody:  &cappedBuffer{},
	}
}

func TestProgressFromStatePut(t *testing.T) {
	got := progressFrom(stateExchange("PUT", stateURI, 200, stateBody))
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	p := got[0]
	if p.Percent != 6 || p.Status != "Reading" || p.LastModified != "2026-11-02T15:23:39Z" ||
		p.Source != "OEBPS/text/9780061801150_Bodymatter.xhtml" {
		t.Fatalf("wrong event for %s: %d %s", p.EntitlementID, p.Percent, p.Status)
	}
	for _, name := range []string{"Authorization", "X-Kobo-Deviceid", "User-Agent", "X-New-Header"} {
		if p.Headers.Get(name) == "" {
			t.Errorf("%s not kept", name)
		}
	}
	for _, name := range []string{"X-Forwarded-For", "Content-Length", "Cookie", "Accept-Encoding"} {
		if p.Headers.Get(name) != "" {
			t.Errorf("%s kept", name)
		}
	}
}

func TestProgressIgnoresOtherExchanges(t *testing.T) {
	cases := map[string]*Exchange{
		"get":         stateExchange("GET", stateURI, 200, stateBody),
		"failed":      stateExchange("PUT", stateURI, 500, stateBody),
		"other path":  stateExchange("PUT", "/kobo/tok/v1/library/sync", 200, stateBody),
		"bad json":    stateExchange("PUT", stateURI, 200, "nope"),
		"id mismatch": stateExchange("PUT", "/kobo/tok/v1/library/00000000-0000-0000-0000-000000000000/state", 200, stateBody),
	}
	for name, e := range cases {
		if got := progressFrom(e); len(got) != 0 {
			t.Errorf("%s: got %d events", name, len(got))
		}
	}
}

type panicObserver struct{}

func (panicObserver) Progress(Progress)     { panic("boom") }
func (panicObserver) Rating(Rating)         { panic("boom") }
func (panicObserver) SyncedBook(SyncedBook) { panic("boom") }

func TestObserverPanicIsContained(t *testing.T) {
	r := &Recorder{Observer: panicObserver{}}
	r.observe(stateExchange("PUT", stateURI, 200, stateBody))
}

func TestRatingIgnoresResponseStatus(t *testing.T) {
	uri := "/kobo/tok/v1/products/c1b7071d-72cd-4472-bdfa-348cb011fed1/rating/4"
	got, ok := ratingFrom(stateExchange("POST", uri, 404, ""))
	if !ok || got.Stars != 4 || got.ProductID != "c1b7071d-72cd-4472-bdfa-348cb011fed1" {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if _, ok := ratingFrom(stateExchange("GET", uri, 200, "")); ok {
		t.Error("GET counted as a rating")
	}
	if _, ok := ratingFrom(stateExchange("POST", "/kobo/tok/v1/products/c1b7071d-72cd-4472-bdfa-348cb011fed1/rating/9", 200, "")); ok {
		t.Error("9 stars accepted")
	}
}

func TestSyncedBooks(t *testing.T) {
	e := stateExchange("GET", "/kobo/tok/v1/library/sync?Filter=ALL", 200, "")
	e.ResponseHeader = http.Header{}
	e.ResponseBody = &cappedBuffer{limit: maxCapturedBody}
	e.ResponseBody.Write([]byte(`[
		{"NewEntitlement":{"BookMetadata":{"EntitlementId":"a","RevisionId":"a","Title":"The monster-god of Mamurth","Contributors":["Edmond Hamilton"]}}},
		{"ChangedEntitlement":{"BookMetadata":{"EntitlementId":"b","RevisionId":"r","Title":"Mort","Isbn":"9780061020681"}}},
		{"ChangedReadingState":{"ReadingState":{"EntitlementId":"c"}}}
	]`))
	got := syncedBooksFrom(e)
	if len(got) != 2 || got[0].Title != "The monster-god of Mamurth" || got[0].Authors[0] != "Edmond Hamilton" || got[1].ISBN != "9780061020681" || got[1].RevisionID != "r" {
		t.Fatalf("got %+v", got)
	}
}
