package wanted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenLibrary finds the work LazyLibrarian knows a book by, since LazyLibrarian
// uses OpenLibrary work ids as its book ids.
type OpenLibrary struct {
	base string
	http *http.Client
}

func NewOpenLibrary() *OpenLibrary {
	return &OpenLibrary{base: "https://openlibrary.org", http: &http.Client{Timeout: 30 * time.Second}}
}

// Work is an OpenLibrary work, with what's needed to check it's the right one.
type Work struct {
	ID      string // e.g. OL24593432W
	Title   string
	Authors []string
}

// ByISBN returns the work an edition with this ISBN belongs to.
func (o *OpenLibrary) ByISBN(ctx context.Context, isbn string) (Work, bool, error) {
	var edition struct {
		Works []struct {
			Key string `json:"key"`
		} `json:"works"`
	}
	found, err := o.get(ctx, "/isbn/"+url.PathEscape(isbn)+".json", &edition)
	if err != nil || !found || len(edition.Works) == 0 {
		return Work{}, false, err
	}
	return o.work(ctx, edition.Works[0].Key)
}

// Search returns the works a title and author search finds.
func (o *OpenLibrary) Search(ctx context.Context, title, author string) ([]Work, error) {
	var out struct {
		Docs []struct {
			Key        string   `json:"key"`
			Title      string   `json:"title"`
			AuthorName []string `json:"author_name"`
		} `json:"docs"`
	}
	query := url.Values{"title": {title}, "fields": {"key,title,author_name"}, "limit": {"5"}}
	if author != "" { // an empty author gets a 500
		query.Set("author", author)
	}
	path := "/search.json?" + query.Encode()
	if _, err := o.get(ctx, path, &out); err != nil {
		return nil, err
	}
	var works []Work
	for _, d := range out.Docs {
		works = append(works, Work{ID: strings.TrimPrefix(d.Key, "/works/"), Title: d.Title, Authors: d.AuthorName})
	}
	return works, nil
}

func (o *OpenLibrary) work(ctx context.Context, key string) (Work, bool, error) {
	var work struct {
		Title   string `json:"title"`
		Authors []struct {
			Author struct {
				Key string `json:"key"`
			} `json:"author"`
		} `json:"authors"`
	}
	found, err := o.get(ctx, key+".json", &work)
	if err != nil || !found {
		return Work{}, false, err
	}
	w := Work{ID: strings.TrimPrefix(key, "/works/"), Title: work.Title}
	for _, a := range work.Authors {
		var author struct {
			Name string `json:"name"`
		}
		if ok, err := o.get(ctx, a.Author.Key+".json", &author); err != nil {
			return Work{}, false, err
		} else if ok {
			w.Authors = append(w.Authors, author.Name)
		}
	}
	return w, true, nil
}

// get returns false for a 404, so a missing edition isn't an error.
func (o *OpenLibrary) get(ctx context.Context, path string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "kobo-progress-shim (https://github.com/fabianvf/kobo-progress-shim)")
	resp, err := o.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("openlibrary: status %d", resp.StatusCode)
	}
	return true, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// LazyLibrarian adds and queues books through its API. The key is in the URL,
// so errors here never carry it.
type LazyLibrarian struct {
	base *url.URL
	key  string
	http *http.Client
}

func NewLazyLibrarian(base *url.URL, key string) *LazyLibrarian {
	return &LazyLibrarian{base: base, key: key, http: &http.Client{Timeout: 2 * time.Minute}}
}

// Statuses returns the eBook status of every book LazyLibrarian has, by BookID.
func (l *LazyLibrarian) Statuses(ctx context.Context) (map[string]string, error) {
	data, err := l.call(ctx, "getAllBooks", nil)
	if err != nil {
		return nil, err
	}
	var books []struct {
		BookID string `json:"BookID"`
		Status string `json:"Status"`
	}
	if err := json.Unmarshal(data, &books); err != nil {
		return nil, errors.New("lazylibrarian: getAllBooks: unexpected response")
	}
	statuses := map[string]string{}
	for _, b := range books {
		statuses[b.BookID] = b.Status
	}
	return statuses, nil
}

// Add puts one book in LazyLibrarian's database, waiting until it's there.
// source is one of LazyLibrarian's book sources, "" for its default.
func (l *LazyLibrarian) Add(ctx context.Context, id, source string) error {
	args := url.Values{"id": {id}, "wait": {"1"}}
	if source != "" {
		args.Set("source", source)
	}
	data, err := l.call(ctx, "addBook", args)
	if err != nil {
		return err
	}
	if msg := strings.Trim(strings.TrimSpace(string(data)), `"`); strings.HasPrefix(msg, "Invalid") || strings.Contains(msg, "disabled") {
		return fmt.Errorf("lazylibrarian: addBook: %.80s", msg)
	}
	return nil
}

// Queue marks a book Wanted, which its next search picks up.
func (l *LazyLibrarian) Queue(ctx context.Context, id string) error {
	data, err := l.call(ctx, "queueBook", url.Values{"id": {id}})
	if err != nil {
		return err
	}
	if strings.TrimSpace(strings.Trim(string(data), `"`)) != "OK" {
		return fmt.Errorf("lazylibrarian: queueBook: %.80s", data)
	}
	return nil
}

func (l *LazyLibrarian) call(ctx context.Context, cmd string, args url.Values) ([]byte, error) {
	q := url.Values{"apikey": {l.key}, "cmd": {cmd}}
	for k, v := range args {
		q[k] = v
	}
	target := l.base.JoinPath("api")
	target.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("lazylibrarian: bad request")
	}
	resp, err := l.http.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("lazylibrarian: %s: %w", cmd, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("lazylibrarian: %s: reading response", cmd)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lazylibrarian: %s: status %d", cmd, resp.StatusCode)
	}
	return data, nil
}
