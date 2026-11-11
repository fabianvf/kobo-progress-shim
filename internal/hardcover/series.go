package hardcover

import (
	"context"
	"strings"
)

// SeriesBook is one book in a series, at its place in the reading order.
type SeriesBook struct {
	ID       int
	Title    string
	Position float64
	Readers  int    // Hardcover users with it on a shelf, to choose between duplicates
	Released string // YYYY-MM-DD, "" if Hardcover doesn't know
	Language string // of the default ebook edition, ISO 639-2, "" if unknown
}

type Series struct {
	ID    int
	Name  string
	Books []SeriesBook
}

// Series returns every series with a book the account has read or is reading.
// Omnibuses and books Hardcover has merged into another are left out, as are
// entries with no position.
func (c *Client) Series(ctx context.Context) ([]Series, error) {
	const q = `query {
		me { user_books(where: {status_id: {_in: [2, 3]}}) { book { book_series { series {
			id name
			book_series(where: {position: {_is_null: false}, book: {compilation: {_eq: false}, canonical_id: {_is_null: true}}},
				order_by: {position: asc}) {
				position book { id title users_count release_date default_ebook_edition { language { code3 } } }
			}
		} } } } }
	}`
	var out struct {
		Me []struct {
			UserBooks []struct {
				Book *struct {
					BookSeries []struct {
						Series *struct {
							ID         int    `json:"id"`
							Name       string `json:"name"`
							BookSeries []struct {
								Position float64 `json:"position"`
								Book     *struct {
									ID       int     `json:"id"`
									Title    string  `json:"title"`
									Users    int     `json:"users_count"`
									Released *string `json:"release_date"`
									Ebook    *struct {
										Language *struct {
											Code3 string `json:"code3"`
										} `json:"language"`
									} `json:"default_ebook_edition"`
								} `json:"book"`
							} `json:"book_series"`
						} `json:"series"`
					} `json:"book_series"`
				} `json:"book"`
			} `json:"user_books"`
		} `json:"me"`
	}
	if err := c.query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var all []Series
	for _, me := range out.Me {
		for _, ub := range me.UserBooks {
			if ub.Book == nil {
				continue
			}
			for _, bs := range ub.Book.BookSeries {
				if bs.Series == nil || seen[bs.Series.ID] {
					continue
				}
				seen[bs.Series.ID] = true
				s := Series{ID: bs.Series.ID, Name: bs.Series.Name}
				for _, e := range bs.Series.BookSeries {
					if e.Book == nil {
						continue
					}
					b := SeriesBook{ID: e.Book.ID, Title: e.Book.Title, Position: e.Position, Readers: e.Book.Users}
					if e.Book.Released != nil {
						b.Released = *e.Book.Released
					}
					if e.Book.Ebook != nil && e.Book.Ebook.Language != nil {
						b.Language = strings.ToLower(e.Book.Ebook.Language.Code3)
					}
					s.Books = append(s.Books, b)
				}
				all = append(all, s)
			}
		}
	}
	return all, nil
}

// WantToRead puts a book the account doesn't have on any shelf on Want to Read.
// Like any shelf change, Hardcover may show it on the reader's activity feed.
func (c *Client) WantToRead(ctx context.Context, bookID int) error {
	return c.insertUserBook(ctx, bookID, StatusWantToRead, nil)
}
