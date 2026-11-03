package match

import (
	"context"
	"errors"
	"testing"
)

type fakeLookup struct {
	byISBN []Book
	search []Book
	err    error
}

func (f fakeLookup) BooksByISBN(context.Context, string) ([]Book, error) { return f.byISBN, f.err }
func (f fakeLookup) Search(context.Context, string, string) ([]Book, error) {
	return f.search, f.err
}

const isbn = "9780061801150"

func TestDecide(t *testing.T) {
	light := Book{ID: 1, Title: "The Light Fantastic", Authors: []string{"Terry Pratchett"}}
	other := Book{ID: 2, Title: "The Light Fantastic", Authors: []string{"Terry Pratchett"}}
	sig := Signals{ISBN: isbn, Title: "The Light Fantastic", Authors: []string{"Terry Pratchett"}}
	noISBN := Signals{Title: sig.Title, Authors: sig.Authors}

	cases := []struct {
		name    string
		lookup  fakeLookup
		signals Signals
		outcome Outcome
		bookID  int
	}{
		{"hardcover id wins", fakeLookup{}, Signals{HardcoverID: 9}, Automatic, 9},
		{"isbn and title agree", fakeLookup{byISBN: []Book{light}}, sig, Automatic, 1},
		{"isbn alone is not enough", fakeLookup{byISBN: []Book{light}}, Signals{ISBN: isbn}, Review, 0},
		{"isbn disagrees", fakeLookup{byISBN: []Book{{ID: 3, Title: "Mort", Authors: []string{"Terry Pratchett"}}}}, sig, Review, 0},
		{"isbn finds two books", fakeLookup{byISBN: []Book{light, other}}, sig, Review, 0},
		{"isbn miss falls back to search", fakeLookup{search: []Book{light}}, sig, Automatic, 1},
		{"one exact search match", fakeLookup{search: []Book{light, {ID: 4, Title: "Light", Authors: []string{"M. John Harrison"}}}}, noISBN, Automatic, 1},
		{"two exact search matches", fakeLookup{search: []Book{light, other}}, noISBN, Review, 0},
		{"same title other author", fakeLookup{search: []Book{{ID: 5, Title: "The Light Fantastic", Authors: []string{"Someone Else"}}}}, noISBN, Review, 0},
		{"search with no authors", fakeLookup{search: []Book{{ID: 6, Title: "The Light Fantastic"}}}, noISBN, Review, 0},
		{"bundle by kobo title", fakeLookup{byISBN: []Book{light}}, Signals{ISBN: isbn, Title: "Discworld eBook Bundle", Authors: sig.Authors}, Review, 0},
		{"bundle by hardcover title", fakeLookup{byISBN: []Book{{ID: 7, Title: "Discworld: Books 1-5", Authors: sig.Authors}}}, sig, Review, 0},
		{"no signals", fakeLookup{}, Signals{}, Review, 0},
		{"whole title beats a split", fakeLookup{search: []Book{
			{ID: 8, Title: "Words of Radiance", Authors: []string{"Brandon Sanderson"}},
			{ID: 9, Title: "Words of Radiance (5 of 5) [Dramatized Adaptation]", Authors: []string{"Brandon Sanderson"}},
		}}, Signals{Title: "Words of Radiance", Authors: []string{"Brandon Sanderson"}}, Automatic, 8},
		{"whole title beats a sibling subtitle", fakeLookup{search: []Book{
			{ID: 10, Title: "Mistborn: Secret History", Authors: []string{"Brandon Sanderson"}},
			{ID: 11, Title: "Mistborn: Collected Tales", Authors: []string{"Brandon Sanderson"}},
		}}, Signals{Title: "Mistborn: Secret History", Authors: []string{"Brandon Sanderson"}}, Automatic, 10},
		{"only splits stays review", fakeLookup{search: []Book{
			{ID: 12, Title: "The Final Empire (1 of 3)", Authors: []string{"Brandon Sanderson"}},
			{ID: 13, Title: "The Final Empire (2 of 3)", Authors: []string{"Brandon Sanderson"}},
		}}, Signals{Title: "The Final Empire", Authors: []string{"Brandon Sanderson"}}, Review, 0},
		{"true duplicates stay review", fakeLookup{search: []Book{light, other}}, noISBN, Review, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Decide(context.Background(), c.lookup, c.signals)
			if err != nil {
				t.Fatal(err)
			}
			if d.Outcome != c.outcome || d.BookID != c.bookID {
				t.Fatalf("got %s book %d (%s), want %s book %d", d.Outcome, d.BookID, d.Reason, c.outcome, c.bookID)
			}
		})
	}
}

func TestDecideReturnsLookupErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, s := range []Signals{{ISBN: isbn}, {Title: "Mort"}} {
		if _, err := Decide(context.Background(), fakeLookup{err: boom}, s); !errors.Is(err, boom) {
			t.Fatalf("signals %+v: got %v", s, err)
		}
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"The Frugal Wizard’s Handbook":                "frugal wizards handbook",
		"The Frugal Wizard's Handbook":                "frugal wizards handbook",
		"Hammered (with bonus short story)":           "hammered",
		"Grimoire of the Lamb: An Iron Druid Novella": "grimoire of the lamb",
		"The Grimoire of the Lamb":                    "grimoire of the lamb",
		"Táin":                                        "tain",
		"Juniper & Thorn":                             "juniper thorn",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestISBNFromPath(t *testing.T) {
	cases := map[string]string{
		"OEBPS/text/9780061801150_Bodymatter.xhtml": isbn,
		"OEBPS/wrap0000.xhtml":                      "",
		"OEBPS/9780061801151_bad_check.xhtml":       "",
	}
	for in, want := range cases {
		if got := ISBNFromPath(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

type shelfLookup struct {
	fakeLookup
	shelf map[int]bool
}

func (s shelfLookup) OnShelf(context.Context, []int) (map[int]bool, error) { return s.shelf, nil }

func TestTheShelvedDuplicateDecides(t *testing.T) {
	a := Book{ID: 338931, Title: "Elantris", Authors: []string{"Brandon Sanderson"}}
	b := Book{ID: 3075568, Title: "Elantris", Authors: []string{"Brandon Sanderson"}}
	sig := Signals{Title: "Elantris", Authors: []string{"Brandon Sanderson"}}
	cases := []struct {
		shelf   map[int]bool
		outcome Outcome
		bookID  int
	}{
		{map[int]bool{338931: true}, Automatic, 338931},
		{map[int]bool{}, Review, 0},
		{map[int]bool{338931: true, 3075568: true}, Review, 0},
	}
	for _, c := range cases {
		d, err := Decide(context.Background(), shelfLookup{fakeLookup{search: []Book{a, b}}, c.shelf}, sig)
		if err != nil || d.Outcome != c.outcome || d.BookID != c.bookID {
			t.Errorf("shelf %v: got %s %d (%s), err %v", c.shelf, d.Outcome, d.BookID, d.Reason, err)
		}
	}
}
