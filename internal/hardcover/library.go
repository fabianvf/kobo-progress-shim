package hardcover

import (
	"context"
	"strings"
)

// Hardcover's status ids, beyond reading and read.
const StatusWantToRead = 1

// ShelfBook is a book on a user's shelf or list, with what's needed to find it
// elsewhere: its title and authors to match, and ISBNs to fetch it by.
type ShelfBook struct {
	ID       int
	Title    string
	Authors  []string
	ISBNs    []string // the default ebook edition first, then the physical one
	Language string   // of the default ebook edition, ISO 639-2, "" if unknown
}

type List struct {
	ID      int
	Name    string
	BookIDs []int
}

// Library is one user's shelves and lists.
type Library struct {
	Status   map[int]int     // book id to status id
	Progress map[int]float64 // percent through the latest read, for books being read
	Books    map[int]ShelfBook
	Lists    []List
}

type bookJSON struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	Contributions []struct {
		Author *struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"contributions"`
	Ebook *struct {
		ISBN13   *string `json:"isbn_13"`
		Language *struct {
			Code3 string `json:"code3"`
		} `json:"language"`
	} `json:"default_ebook_edition"`
	Physical *struct {
		ISBN13 *string `json:"isbn_13"`
	} `json:"default_physical_edition"`
}

func (b bookJSON) shelfBook() ShelfBook {
	s := ShelfBook{ID: b.ID, Title: b.Title}
	for _, c := range b.Contributions {
		if c.Author != nil {
			s.Authors = append(s.Authors, c.Author.Name)
		}
	}
	if b.Ebook != nil {
		if b.Ebook.ISBN13 != nil && *b.Ebook.ISBN13 != "" {
			s.ISBNs = append(s.ISBNs, *b.Ebook.ISBN13)
		}
		if b.Ebook.Language != nil {
			s.Language = strings.ToLower(b.Ebook.Language.Code3)
		}
	}
	if b.Physical != nil && b.Physical.ISBN13 != nil && *b.Physical.ISBN13 != "" {
		s.ISBNs = append(s.ISBNs, *b.Physical.ISBN13)
	}
	return s
}

// Library reads the account's shelves and lists in one query.
func (c *Client) Library(ctx context.Context) (Library, error) {
	const book = `id title contributions { author { name } }
		default_ebook_edition { isbn_13 language { code3 } }
		default_physical_edition { isbn_13 }`
	q := `query {
		me {
			user_books {
				status_id book { ` + book + ` }
				user_book_reads(order_by: {id: desc}, limit: 1) { progress finished_at }
			}
			lists { id name list_books { book { ` + book + ` } } }
		}
	}`
	var out struct {
		Me []struct {
			UserBooks []struct {
				StatusID int       `json:"status_id"`
				Book     *bookJSON `json:"book"`
				Reads    []struct {
					Progress   *float64 `json:"progress"`
					FinishedAt *string  `json:"finished_at"`
				} `json:"user_book_reads"`
			} `json:"user_books"`
			Lists []struct {
				ID        int    `json:"id"`
				Name      string `json:"name"`
				ListBooks []struct {
					Book *bookJSON `json:"book"`
				} `json:"list_books"`
			} `json:"lists"`
		} `json:"me"`
	}
	if err := c.query(ctx, q, nil, &out); err != nil {
		return Library{}, err
	}
	lib := Library{Status: map[int]int{}, Progress: map[int]float64{}, Books: map[int]ShelfBook{}}
	for _, me := range out.Me {
		for _, ub := range me.UserBooks {
			if ub.Book == nil {
				continue
			}
			lib.Status[ub.Book.ID] = ub.StatusID
			lib.Books[ub.Book.ID] = ub.Book.shelfBook()
			if len(ub.Reads) > 0 && ub.Reads[0].FinishedAt == nil && ub.Reads[0].Progress != nil {
				lib.Progress[ub.Book.ID] = *ub.Reads[0].Progress
			}
		}
		for _, l := range me.Lists {
			list := List{ID: l.ID, Name: l.Name}
			for _, lb := range l.ListBooks {
				if lb.Book == nil {
					continue
				}
				list.BookIDs = append(list.BookIDs, lb.Book.ID)
				if _, ok := lib.Books[lb.Book.ID]; !ok {
					lib.Books[lb.Book.ID] = lb.Book.shelfBook()
				}
			}
			lib.Lists = append(lib.Lists, list)
		}
	}
	return lib, nil
}
