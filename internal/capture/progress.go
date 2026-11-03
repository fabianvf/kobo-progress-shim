package capture

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
)

// Progress is one book's state from a successful state PUT. Headers holds the
// Kobo's own auth, for a store lookup in memory. It must never be logged or
// stored, so don't print this struct whole.
type Progress struct {
	User          string // accounts.Key of the CWA token in the path
	EntitlementID string
	Percent       int
	Status        string
	Source        string
	LastModified  string
	Headers       http.Header
}

// deviceHeaders keeps everything the device sent, as CWA does when it forwards a
// request to the store, except what describes the hop to this proxy, CWA's own
// session, or the PUT's body. Accept-Encoding goes too, because setting it would
// stop Go decoding the response.
func deviceHeaders(in http.Header) http.Header {
	out := http.Header{}
	for name, values := range in {
		switch {
		case strings.HasPrefix(name, "X-Forwarded-"), strings.HasPrefix(name, "Content-"):
		case name == "Forwarded", name == "X-Real-Ip", name == "Cookie", name == "Accept-Encoding", name == "Connection":
		default:
			out[name] = values
		}
	}
	return out
}

var statePath = regexp.MustCompile(`^/kobo/([^/]+)/v1/library/([0-9a-fA-F-]{36})/state(\?|$)`)

// progressFrom returns what a state PUT reports, or nothing for any other
// exchange. A failed PUT is ignored because the Kobo will send it again.
func progressFrom(e *Exchange) []Progress {
	if e.Method != "PUT" || e.Status < 200 || e.Status >= 300 {
		return nil
	}
	path := statePath.FindStringSubmatch(e.RequestURI)
	if path == nil {
		return nil
	}
	raw, truncated := e.RequestBody.snapshot()
	if truncated {
		return nil
	}
	var body struct {
		ReadingStates []struct {
			EntitlementID   string `json:"EntitlementId"`
			CurrentBookmark *struct {
				ProgressPercent float64 `json:"ProgressPercent"`
				Location        *struct {
					Source string `json:"Source"`
				} `json:"Location"`
			} `json:"CurrentBookmark"`
			StatusInfo *struct {
				Status       string `json:"Status"`
				LastModified string `json:"LastModified"`
			} `json:"StatusInfo"`
		} `json:"ReadingStates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	var out []Progress
	for _, state := range body.ReadingStates {
		if state.EntitlementID != path[2] {
			continue
		}
		p := Progress{
			User:          accounts.Key(path[1]),
			EntitlementID: state.EntitlementID,
			Headers:       deviceHeaders(e.RequestHeader),
		}
		if state.CurrentBookmark != nil {
			p.Percent = int(state.CurrentBookmark.ProgressPercent)
			if state.CurrentBookmark.Location != nil {
				p.Source = state.CurrentBookmark.Location.Source
			}
		}
		if state.StatusInfo != nil {
			p.Status = state.StatusInfo.Status
			p.LastModified = state.StatusInfo.LastModified
		}
		out = append(out, p)
	}
	return out
}

// Rating is a star rating the Kobo sent. ProductID is the store's RevisionId,
// which isn't the EntitlementId on store and Libby books.
type Rating struct {
	User      string
	ProductID string
	Stars     int
}

var ratingPath = regexp.MustCompile(`^/kobo/([^/]+)/v1/products/([0-9a-fA-F-]{36})/rating/([1-5])(\?|$)`)

// ratingFrom ignores the response status. CWA answers 404 for its own books, and
// the rating is still what the reader chose.
func ratingFrom(e *Exchange) (Rating, bool) {
	if e.Method != "POST" {
		return Rating{}, false
	}
	m := ratingPath.FindStringSubmatch(e.RequestURI)
	if m == nil {
		return Rating{}, false
	}
	return Rating{User: accounts.Key(m[1]), ProductID: m[2], Stars: int(m[3][0] - '0')}, true
}

// SyncedBook is a book's metadata from a sync response. It's the only source for
// CWA's books, which the store doesn't know.
type SyncedBook struct {
	User          string
	EntitlementID string
	RevisionID    string
	Title         string
	Authors       []string
	ISBN          string
}

var syncPath = regexp.MustCompile(`^/kobo/([^/]+)/v1/library/sync(\?|$)`)

func syncedBooksFrom(e *Exchange) []SyncedBook {
	path := syncPath.FindStringSubmatch(e.RequestURI)
	if e.Method != "GET" || e.Status != 200 || path == nil {
		return nil
	}
	raw, truncated := e.ResponseBody.snapshot()
	if truncated {
		return nil
	}
	raw, err := decoded(raw, e.ResponseHeader)
	if err != nil {
		return nil
	}
	type entitlement struct {
		BookMetadata *struct {
			EntitlementID string   `json:"EntitlementId"`
			RevisionID    string   `json:"RevisionId"`
			Title         string   `json:"Title"`
			Contributors  []string `json:"Contributors"`
			ISBN          string   `json:"Isbn"`
		} `json:"BookMetadata"`
	}
	var entries []struct {
		New     *entitlement `json:"NewEntitlement"`
		Changed *entitlement `json:"ChangedEntitlement"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	var out []SyncedBook
	for _, entry := range entries {
		item := entry.New
		if item == nil {
			item = entry.Changed
		}
		if item == nil || item.BookMetadata == nil || item.BookMetadata.EntitlementID == "" {
			continue
		}
		m := item.BookMetadata
		out = append(out, SyncedBook{User: accounts.Key(path[1]), EntitlementID: m.EntitlementID, RevisionID: m.RevisionID, Title: m.Title, Authors: m.Contributors, ISBN: m.ISBN})
	}
	return out
}

// Observer receives what the Kobo reports. Its methods must not block.
type Observer interface {
	Progress(Progress)
	Rating(Rating)
	SyncedBook(SyncedBook)
}

func (r *Recorder) observe(exchange *Exchange) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("observer failed: %T", recovered)
		}
	}()
	if r.Observer == nil {
		return
	}
	for _, b := range syncedBooksFrom(exchange) {
		r.Observer.SyncedBook(b)
	}
	for _, p := range progressFrom(exchange) {
		r.Observer.Progress(p)
	}
	if rating, ok := ratingFrom(exchange); ok {
		r.Observer.Rating(rating)
	}
}
