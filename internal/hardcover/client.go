// Package hardcover talks to Hardcover's GraphQL API.
package hardcover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/match"
)

const endpoint = "https://api.hardcover.app/v1/graphql"

// ErrRejected means Hardcover refused the key, answering 401 with "Invalid or
// expired token". Its API doesn't say when a key will expire, so this is the
// only way to know.
var ErrRejected = errors.New("hardcover: key rejected, invalid or expired")

// Hardcover documents a limit of 60 requests a minute.
const minInterval = 1100 * time.Millisecond

type Client struct {
	token    string
	endpoint string
	http     *http.Client
	tick     <-chan time.Time
}

func New(token string) *Client {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = "Bearer " + token
	}
	return &Client{
		token:    token,
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
		tick:     time.Tick(minInterval),
	}
}

func (c *Client) query(ctx context.Context, query string, vars map[string]any, out any) error {
	select {
	case <-c.tick:
	case <-ctx.Done():
		return ctx.Err()
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.token)
	req.Header.Set("User-Agent", "kobo-progress-shim")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrRejected
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hardcover: status %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("hardcover: decoding response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("hardcover: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, out)
}

// BooksByISBN returns each distinct book with an edition carrying this ISBN.
func (c *Client) BooksByISBN(ctx context.Context, isbn string) ([]match.Book, error) {
	const q = `query($i: String!) {
		editions(where: {isbn_13: {_eq: $i}}) {
			book { id title contributions { author { name } } }
		}
	}`
	var out struct {
		Editions []struct {
			Book *struct {
				ID            int    `json:"id"`
				Title         string `json:"title"`
				Contributions []struct {
					Author *struct {
						Name string `json:"name"`
					} `json:"author"`
				} `json:"contributions"`
			} `json:"book"`
		} `json:"editions"`
	}
	if err := c.query(ctx, q, map[string]any{"i": isbn}, &out); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var books []match.Book
	for _, e := range out.Editions {
		if e.Book == nil || seen[e.Book.ID] {
			continue
		}
		seen[e.Book.ID] = true
		b := match.Book{ID: e.Book.ID, Title: e.Book.Title}
		for _, contribution := range e.Book.Contributions {
			if contribution.Author != nil {
				b.Authors = append(b.Authors, contribution.Author.Name)
			}
		}
		books = append(books, b)
	}
	return books, nil
}

func (c *Client) Search(ctx context.Context, title, author string) ([]match.Book, error) {
	const q = `query($q: String!) {
		search(query: $q, query_type: "Book", per_page: 5) { results }
	}`
	var out struct {
		Search struct {
			Results struct {
				Hits []struct {
					Document struct {
						ID          json.Number `json:"id"`
						Title       string      `json:"title"`
						AuthorNames []string    `json:"author_names"`
					} `json:"document"`
				} `json:"hits"`
			} `json:"results"`
		} `json:"search"`
	}
	vars := map[string]any{"q": strings.TrimSpace(title + " " + author)}
	if err := c.query(ctx, q, vars, &out); err != nil {
		return nil, err
	}
	var books []match.Book
	for _, h := range out.Search.Results.Hits {
		id, err := h.Document.ID.Int64()
		if err != nil {
			return nil, fmt.Errorf("hardcover: search id %q: %w", h.Document.ID, err)
		}
		books = append(books, match.Book{ID: int(id), Title: h.Document.Title, Authors: h.Document.AuthorNames})
	}
	return books, nil
}

// Statuses returns the shelf status of each of these books that's on the
// account, keyed by book id. A book missing from the map isn't on any shelf.
func (c *Client) Statuses(ctx context.Context, bookIDs []int) (map[int]int, error) {
	const q = `query($ids: [Int!]) {
		me { user_books(where: {book_id: {_in: $ids}}) { book_id status_id } }
	}`
	var out struct {
		Me []struct {
			UserBooks []struct {
				BookID   int `json:"book_id"`
				StatusID int `json:"status_id"`
			} `json:"user_books"`
		} `json:"me"`
	}
	if err := c.query(ctx, q, map[string]any{"ids": bookIDs}, &out); err != nil {
		return nil, err
	}
	statuses := map[int]int{}
	for _, me := range out.Me {
		for _, ub := range me.UserBooks {
			statuses[ub.BookID] = ub.StatusID
		}
	}
	return statuses, nil
}

// StatusRead is Hardcover's status id for a book that's been read.
const StatusRead = statusRead

// Username checks the token works and returns whose it is.
func (c *Client) Username(ctx context.Context) (string, error) {
	var out struct {
		Me []struct {
			Username string `json:"username"`
		} `json:"me"`
	}
	if err := c.query(ctx, `query { me { username } }`, nil, &out); err != nil {
		return "", err
	}
	if len(out.Me) == 0 || out.Me[0].Username == "" {
		return "", fmt.Errorf("hardcover: the token names no user")
	}
	return out.Me[0].Username, nil
}

// OnShelf reports which of these books are on any of the account's shelves,
// which the matcher uses to choose between Hardcover's duplicates.
func (c *Client) OnShelf(ctx context.Context, bookIDs []int) (map[int]bool, error) {
	statuses, err := c.Statuses(ctx, bookIDs)
	if err != nil {
		return nil, err
	}
	on := map[int]bool{}
	for id := range statuses {
		on[id] = true
	}
	return on, nil
}
