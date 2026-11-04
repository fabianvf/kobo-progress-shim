package cwasync

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxPages bounds the OPDS walk, in case a feed links back to itself.
const maxPages = 100

// CWA reads the library through OPDS as the robot account, and reads and sets a
// user's Kobo reading state through CWA's Kobo API with that user's sync token.
// The token is a secret in a URL path, so errors here never carry the URL.
type CWA struct {
	base     *url.URL
	user     string
	password string
	http     *http.Client
}

func NewCWA(base *url.URL, user, password string) *CWA {
	return &CWA{base: base, user: user, password: password, http: &http.Client{
		Timeout: 30 * time.Second,
		// A Kobo request for a book CWA doesn't hold is redirected to Kobo's
		// store, and that must never be followed with a user's token.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type Book struct {
	UUID    string
	Title   string
	Authors []string
}

type feed struct {
	Entries []struct {
		ID      string `xml:"id"`
		Title   string `xml:"title"`
		Authors []struct {
			Name string `xml:"name"`
		} `xml:"author"`
	} `xml:"entry"`
	Links []struct {
		Rel  string `xml:"rel,attr"`
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

// Books lists the whole library, newest first.
func (c *CWA) Books(ctx context.Context) ([]Book, error) {
	var books []Book
	next := "opds/new"
	for range maxPages {
		target, err := c.base.Parse(next)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(c.user, c.password)
		data, err := c.do(req, "opds")
		if err != nil {
			return nil, err
		}
		var f feed
		if err := xml.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("cwa: opds: %w", err)
		}
		for _, e := range f.Entries {
			b := Book{UUID: strings.TrimPrefix(e.ID, "urn:uuid:"), Title: e.Title}
			for _, a := range e.Authors {
				b.Authors = append(b.Authors, a.Name)
			}
			books = append(books, b)
		}
		next = ""
		for _, l := range f.Links {
			if l.Rel == "next" {
				next = l.Href
			}
		}
		if next == "" || len(f.Entries) == 0 {
			return books, nil
		}
	}
	return nil, errors.New("cwa: opds: too many pages")
}

// State is a book's reading state as CWA's Kobo API gives it.
type State struct {
	Status        string
	Percent       *float64
	SourcePercent *float64
	Located       bool // the bookmark has a place in the book, so the Kobo has opened it
}

type stateJSON struct {
	EntitlementID   string `json:"EntitlementId"`
	CurrentBookmark *struct {
		ProgressPercent              *float64 `json:"ProgressPercent"`
		ContentSourceProgressPercent *float64 `json:"ContentSourceProgressPercent"`
		Location                     *struct {
			Value string `json:"Value"`
		} `json:"Location"`
	} `json:"CurrentBookmark"`
	StatusInfo *struct {
		Status string `json:"Status"`
	} `json:"StatusInfo"`
}

// State reads a user's state for a book. CWA creates an unread state for a book
// that had none, which the Kobo then receives on its next sync.
func (c *CWA) State(ctx context.Context, token, uuid string) (State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.statePath(token, uuid), nil)
	if err != nil {
		return State{}, errors.New("cwa: state: bad request")
	}
	data, err := c.do(req, "state")
	if err != nil {
		return State{}, err
	}
	var states []stateJSON
	if err := json.Unmarshal(data, &states); err != nil || len(states) != 1 || states[0].EntitlementID != uuid {
		return State{}, errors.New("cwa: state: unexpected response")
	}
	var st State
	if s := states[0].StatusInfo; s != nil {
		st.Status = s.Status
	}
	if b := states[0].CurrentBookmark; b != nil {
		st.Percent, st.SourcePercent = b.ProgressPercent, b.ContentSourceProgressPercent
		st.Located = b.Location != nil && b.Location.Value != ""
	}
	return st, nil
}

// SetStatus changes only the status. The bookmark goes back as it came, because
// CWA's handler needs one and would otherwise move the reader's place.
func (c *CWA) SetStatus(ctx context.Context, token, uuid string, current State, status string) error {
	zero := 0.0
	percent, source := current.Percent, current.SourcePercent
	if percent == nil {
		percent = &zero
	}
	if source == nil {
		source = &zero
	}
	body, err := json.Marshal(map[string]any{"ReadingStates": []map[string]any{{
		"EntitlementId":   uuid,
		"CurrentBookmark": map[string]any{"ProgressPercent": *percent, "ContentSourceProgressPercent": *source},
		"Statistics":      nil,
		"StatusInfo":      map[string]any{"Status": status, "LastModified": time.Now().UTC().Format(time.RFC3339)},
	}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.statePath(token, uuid), bytes.NewReader(body))
	if err != nil {
		return errors.New("cwa: state: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = c.do(req, "set state")
	return err
}

func (c *CWA) statePath(token, uuid string) string {
	return c.base.JoinPath("kobo", token, "v1", "library", uuid, "state").String()
}

// do returns the body of a 200. Its errors name the operation, never the URL.
func (c *CWA) do(req *http.Request, op string) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("cwa: %s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("cwa: %s: reading response", op)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cwa: %s: status %d", op, resp.StatusCode)
	}
	return data, nil
}

// ErrGone means CWA doesn't know a collection any more, usually because someone
// deleted it on their Kobo. CWA passes such requests on to Kobo's store, which
// has answered 403 and 401, so those and a 404 count. A 401 from CWA itself, for
// a bad token, would look the same, but the sync's state calls use the token
// first and would have failed.
var ErrGone = errors.New("cwa: collection gone")

func tagItems(uuids []string) []map[string]string {
	items := []map[string]string{}
	for _, u := range uuids {
		items = append(items, map[string]string{"RevisionId": u, "Type": "ProductRevisionTagItem"})
	}
	return items
}

// CreateTag makes a collection, or returns the user's existing one of that name,
// and adds items to it.
func (c *CWA) CreateTag(ctx context.Context, token, name string, items []string) (string, error) {
	data, err := c.tagCall(ctx, http.MethodPost, token, nil, map[string]any{"Name": name, "Items": tagItems(items)}, "create collection")
	if err != nil {
		return "", err
	}
	var id string
	if err := json.Unmarshal(data, &id); err != nil || id == "" {
		return "", errors.New("cwa: create collection: unexpected response")
	}
	return id, nil
}

func (c *CWA) RenameTag(ctx context.Context, token, id, name string) error {
	_, err := c.tagCall(ctx, http.MethodPut, token, []string{id}, map[string]any{"Name": name}, "rename collection")
	return err
}

func (c *CWA) AddToTag(ctx context.Context, token, id string, items []string) error {
	_, err := c.tagCall(ctx, http.MethodPost, token, []string{id, "items"}, map[string]any{"Items": tagItems(items)}, "add to collection")
	return err
}

func (c *CWA) RemoveFromTag(ctx context.Context, token, id string, items []string) error {
	_, err := c.tagCall(ctx, http.MethodPost, token, []string{id, "items", "delete"}, map[string]any{"Items": tagItems(items)}, "remove from collection")
	return err
}

func (c *CWA) DeleteTag(ctx context.Context, token, id string) error {
	_, err := c.tagCall(ctx, http.MethodDelete, token, []string{id}, nil, "delete collection")
	return err
}

func (c *CWA) tagCall(ctx context.Context, method, token string, path []string, body any, op string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	target := c.base.JoinPath(append([]string{"kobo", token, "v1", "library", "tags"}, path...)...)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, errors.New("cwa: " + op + ": bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cwa: %s: %w", op, unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cwa: %s: reading response", op)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusTemporaryRedirect:
		return nil, ErrGone
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("cwa: %s: status %d", op, resp.StatusCode)
	}
	return data, nil
}
