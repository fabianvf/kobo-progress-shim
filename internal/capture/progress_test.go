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
		RequestHeader: http.Header{"Authorization": {"Bearer " + secret}, "User-Agent": {"kobo"}},
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
		p.Source != "OEBPS/text/9780061801150_Bodymatter.xhtml" || p.Authorization != "Bearer "+secret {
		t.Fatalf("wrong event for %s: %d %s", p.EntitlementID, p.Percent, p.Status)
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

func TestObserverPanicIsContained(t *testing.T) {
	r := &Recorder{OnProgress: func(Progress) { panic("boom") }}
	r.observe(stateExchange("PUT", stateURI, 200, stateBody))
}
