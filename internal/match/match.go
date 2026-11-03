// Package match decides which Hardcover book a Kobo book is. A wrong match
// publishes false reading history, so anything uncertain goes to review.
package match

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

type Outcome string

const (
	Automatic Outcome = "automatic"
	Review    Outcome = "review"
)

// Signals is what is known about a book from live traffic.
type Signals struct {
	HardcoverID int
	ISBN        string
	Title       string
	Authors     []string
}

type Book struct {
	ID      int
	Title   string
	Authors []string
}

// Lookup is the read side of Hardcover.
type Lookup interface {
	BooksByISBN(ctx context.Context, isbn string) ([]Book, error)
	Search(ctx context.Context, title, author string) ([]Book, error)
}

type Decision struct {
	Outcome    Outcome
	BookID     int
	Reason     string
	Candidates []Book
}

var bundle = regexp.MustCompile(`(?i)\b(bundle|box(ed)? set|omnibus|books? \d+\s*-\s*\d+)\b`)

// Decide returns an error only when Hardcover couldn't be asked, so a caller can
// retry instead of queueing a review that better data would have avoided.
func Decide(ctx context.Context, l Lookup, s Signals) (Decision, error) {
	if s.HardcoverID != 0 {
		return Decision{Outcome: Automatic, BookID: s.HardcoverID, Reason: "hardcover-id"}, nil
	}

	var candidates []Book
	if ValidISBN13(s.ISBN) {
		books, err := l.BooksByISBN(ctx, s.ISBN)
		if err != nil {
			return Decision{}, err
		}
		candidates = books
		if len(books) > 1 {
			return Decision{Outcome: Review, Reason: "isbn found several books", Candidates: books}, nil
		}
		if len(books) == 1 {
			switch {
			case s.Title == "":
				return Decision{Outcome: Review, Reason: "isbn with no title to check", Candidates: books}, nil
			case isBundle(s.Title, books[0].Title):
				return Decision{Outcome: Review, Reason: "bundle", Candidates: books}, nil
			case Same(s.Title, s.Authors, books[0]):
				return Decision{Outcome: Automatic, BookID: books[0].ID, Reason: "isbn, title and author agree"}, nil
			}
			return Decision{Outcome: Review, Reason: "isbn disagrees with title or author", Candidates: books}, nil
		}
	}

	if s.Title == "" {
		return Decision{Outcome: Review, Reason: "no title", Candidates: candidates}, nil
	}
	if isBundle(s.Title, "") {
		return Decision{Outcome: Review, Reason: "bundle"}, nil
	}

	author := ""
	if len(s.Authors) > 0 {
		author = s.Authors[0]
	}
	// Notes like "(with bonus short story)" pull unrelated books into the results.
	found, err := l.Search(ctx, strings.TrimSpace(bracketed.ReplaceAllString(s.Title, "")), author)
	if err != nil {
		return Decision{}, err
	}
	var exact, whole []Book
	for _, b := range found {
		if Same(s.Title, s.Authors, b) {
			exact = append(exact, b)
			if wholeTitle(s.Title) == wholeTitle(b.Title) {
				whole = append(whole, b)
			}
		}
	}
	// Splits like "(2 of 3)" and books sharing a series name before a colon all
	// look the same once subtitles go, so one match on the whole title decides.
	if len(exact) > 1 && len(whole) == 1 {
		return Decision{Outcome: Automatic, BookID: whole[0].ID, Reason: "one exact search match on the whole title"}, nil
	}
	switch len(exact) {
	case 0:
		return Decision{Outcome: Review, Reason: "no exact search match", Candidates: found}, nil
	case 1:
		return Decision{Outcome: Automatic, BookID: exact[0].ID, Reason: "one exact search match"}, nil
	}
	// Hardcover's duplicates usually share a title and author exactly. The one
	// the reader shelved is the one they mean, since a person put it there.
	if shelf, ok := l.(Shelf); ok {
		ids := make([]int, len(exact))
		for i, b := range exact {
			ids[i] = b.ID
		}
		on, err := shelf.OnShelf(ctx, ids)
		if err != nil {
			return Decision{}, err
		}
		var shelved []Book
		for _, b := range exact {
			if on[b.ID] {
				shelved = append(shelved, b)
			}
		}
		if len(shelved) == 1 {
			return Decision{Outcome: Automatic, BookID: shelved[0].ID, Reason: "the exact match on the reader's shelf"}, nil
		}
	}
	return Decision{Outcome: Review, Reason: "several exact search matches", Candidates: exact}, nil
}

// Shelf is an optional part of a Lookup: which of these books the reader has on
// a Hardcover shelf.
type Shelf interface {
	OnShelf(ctx context.Context, bookIDs []int) (map[int]bool, error)
}

// wholeTitle keeps the subtitle and bracketed notes that NormalizeTitle drops.
func wholeTitle(s string) string {
	return strings.TrimPrefix(folded(s), "the ")
}

// Same reports whether the normalized titles are equal and an author surname is
// shared. Having no author on either side is not agreement.
func Same(title string, authors []string, b Book) bool {
	if NormalizeTitle(title) == "" || NormalizeTitle(title) != NormalizeTitle(b.Title) {
		return false
	}
	for _, a := range authors {
		for _, other := range b.Authors {
			if s := surname(a); s != "" && s == surname(other) {
				return true
			}
		}
	}
	return false
}

func isBundle(titles ...string) bool {
	for _, t := range titles {
		if bundle.MatchString(t) {
			return true
		}
	}
	return false
}

var (
	bracketed  = regexp.MustCompile(`\([^)]*\)|\[[^\]]*\]`)
	apostrophe = strings.NewReplacer("'", "", "’", "", "‘", "")
)

// NormalizeTitle drops subtitles, bracketed notes, accents, punctuation and a
// leading "the", which Hardcover and store listings use inconsistently.
func NormalizeTitle(s string) string {
	s = bracketed.ReplaceAllString(s, "")
	s, _, _ = strings.Cut(s, ":")
	s = folded(s)
	return strings.TrimPrefix(s, "the ")
}

func surname(s string) string {
	fields := strings.Fields(folded(s))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func folded(s string) string {
	s = apostrophe.Replace(s)
	var b strings.Builder
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var isbnInPath = regexp.MustCompile(`97[89]\d{10}`)

// ISBNFromPath finds an ISBN in a bookmark's Location.Source. Publishers often
// name content files after the ISBN, but not reliably.
func ISBNFromPath(path string) string {
	for _, m := range isbnInPath.FindAllString(path, -1) {
		if ValidISBN13(m) {
			return m
		}
	}
	return ""
}

func ValidISBN13(s string) bool {
	if len(s) != 13 {
		return false
	}
	sum := 0
	for i, r := range s {
		if r < '0' || r > '9' {
			return false
		}
		d := int(r - '0')
		if i%2 == 1 {
			d *= 3
		}
		sum += d
	}
	return sum%10 == 0
}
