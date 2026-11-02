// match-eval measures the matcher against an answer key: a CSV of books with
// kobo_title, kobo_author, isbn and path_isbn columns. Each scenario gives the
// matcher only the signals that route would provide live.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
)

type row struct {
	title, author, isbn, pathISBN string
}

var scenarios = []struct {
	name    string
	signals func(row) match.Signals
}{
	{"store lookup returns isbn, title, author", func(r row) match.Signals {
		return match.Signals{ISBN: r.isbn, Title: r.title, Authors: authors(r.author)}
	}},
	{"title and author only", func(r row) match.Signals {
		return match.Signals{Title: r.title, Authors: authors(r.author)}
	}},
	{"bookmark path only", func(r row) match.Signals {
		return match.Signals{ISBN: r.pathISBN}
	}},
}

var authorSplit = regexp.MustCompile(`,|&| and `)

func authors(s string) []string {
	var out []string
	for _, a := range authorSplit.Split(s, -1) {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func main() {
	key := flag.String("key", "", "answer key CSV")
	flag.Parse()
	token := os.Getenv("HARDCOVER_API_KEY")
	if token == "" || *key == "" {
		log.Fatal("need -key and HARDCOVER_API_KEY")
	}
	rows, err := readKey(*key)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	client := hardcover.New(token)

	// The full scenario's automatic answers check the weaker scenarios, because
	// they rest on an ISBN and a title and author agreeing.
	want := map[string]int{}
	for i, s := range scenarios {
		fmt.Printf("\n== %s\n", s.name)
		var automatic, review, wrong int
		for _, r := range rows {
			var d match.Decision
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				if d, err = match.Decide(ctx, client, s.signals(r)); err == nil {
					break
				}
			}
			if err != nil {
				log.Fatalf("%s: %v", r.title, err)
			}
			if d.Outcome == match.Review {
				review++
				suggestion := "none"
				if len(d.Candidates) > 0 {
					suggestion = fmt.Sprintf("%d %q", d.Candidates[0].ID, d.Candidates[0].Title)
				}
				fmt.Printf("  review  %-45.45s %-35s suggests %s\n", r.title, d.Reason, suggestion)
				continue
			}
			automatic++
			if i == 0 {
				want[r.title] = d.BookID
				continue
			}
			if id, ok := want[r.title]; ok && id != d.BookID {
				wrong++
				fmt.Printf("  WRONG   %-45.45s got %d, key has %d\n", r.title, d.BookID, id)
			}
		}
		fmt.Printf("  automatic %d, review %d, conflicts with first scenario %d, of %d\n", automatic, review, wrong, len(rows))
	}
}

func readKey(path string) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	var rows []row
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		pathISBN, _, _ := strings.Cut(rec[col["path_isbn"]], ",")
		rows = append(rows, row{
			title:    rec[col["kobo_title"]],
			author:   rec[col["kobo_author"]],
			isbn:     rec[col["isbn"]],
			pathISBN: pathISBN,
		})
	}
}
