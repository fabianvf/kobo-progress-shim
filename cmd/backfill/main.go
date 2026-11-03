// backfill adds reading history from before the shim existed: books a copy of
// KoboReader.sqlite has as finished that Hardcover doesn't have as read. It's a
// one-off run by hand, never part of the live path, and nothing is written until
// a person has reviewed the plan.
//
//	backfill -db KoboReader.sqlite -plan plan.csv   match and write the plan
//	backfill -apply plan.csv                        write the rows marked yes
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/match"
	_ "modernc.org/sqlite"
)

var header = []string{"write", "kobo_title", "kobo_author", "isbn", "started", "finished", "date_from", "outcome", "hc_book", "hc_title", "hc_status", "note"}

type finished struct {
	title, author, isbn, started, finished, dateFrom string
}

func main() {
	db := flag.String("db", "", "copy of KoboReader.sqlite, opened read-only")
	plan := flag.String("plan", "", "where to write the plan")
	apply := flag.String("apply", "", "reviewed plan to write to Hardcover")
	flag.Parse()
	token := os.Getenv("HARDCOVER_API_KEY")
	if token == "" || (*apply == "") == (*db == "" || *plan == "") {
		log.Fatal("need HARDCOVER_API_KEY, and either -db with -plan or -apply")
	}
	client := hardcover.New(token)
	ctx := context.Background()
	var err error
	if *apply != "" {
		err = write(ctx, client, *apply)
	} else {
		err = makePlan(ctx, client, *db, *plan)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func readKobo(path string) ([]finished, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// ContentType 6 is a book, ReadStatus 2 finished.
	rows, err := db.Query(`SELECT Title, coalesce(Attribution, ''), coalesce(ISBN, ''),
		coalesce(LastTimeStartedReading, ''), coalesce(LastTimeFinishedReading, ''), coalesce(DateLastRead, '')
		FROM content WHERE ContentType = 6 AND ReadStatus = 2 ORDER BY LastTimeFinishedReading`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []finished
	for rows.Next() {
		var f finished
		var lastRead string
		if err := rows.Scan(&f.title, &f.author, &f.isbn, &f.started, &f.finished, &lastRead); err != nil {
			return nil, err
		}
		f.dateFrom = "finished"
		if day(f.finished) == "" {
			f.finished, f.dateFrom = lastRead, "last opened"
		}
		f.started, f.finished = day(f.started), day(f.finished)
		out = append(out, f)
	}
	return out, rows.Err()
}

// day is the UTC date of a Kobo timestamp, the same rule live reports use.
func day(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
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

// makePlan marks a row yes only when the match is automatic, Hardcover doesn't
// have it as read, and there's a date. Everything else is listed for a person to
// decide, and can be changed to yes with a hc_book filled in.
func makePlan(ctx context.Context, client *hardcover.Client, dbPath, planPath string) error {
	books, err := readKobo(dbPath)
	if err != nil {
		return err
	}
	decisions := make([]match.Decision, len(books))
	var ids []int
	for i, b := range books {
		if decisions[i], err = match.Decide(ctx, client, match.Signals{ISBN: b.isbn, Title: b.title, Authors: authors(b.author)}); err != nil {
			return fmt.Errorf("%s: %w", b.title, err)
		}
		if decisions[i].Outcome == match.Automatic {
			ids = append(ids, decisions[i].BookID)
		} else if len(decisions[i].Candidates) > 0 {
			ids = append(ids, decisions[i].Candidates[0].ID) // so a reviewer sees its shelf too
		}
	}
	statuses, err := client.Statuses(ctx, ids)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(planPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	out := csv.NewWriter(f)
	out.Write(header)
	var yes int
	for i, b := range books {
		d := decisions[i]
		row := map[string]string{"write": "no", "kobo_title": b.title, "kobo_author": b.author, "isbn": b.isbn,
			"started": b.started, "finished": b.finished, "date_from": b.dateFrom, "outcome": string(d.Outcome), "note": d.Reason}
		book := match.Book{ID: d.BookID}
		if d.Outcome == match.Review && len(d.Candidates) > 0 {
			book = d.Candidates[0]
			row["note"] = d.Reason + ", suggestion only"
		}
		for _, c := range d.Candidates {
			if c.ID == book.ID {
				book = c
			}
		}
		if book.ID != 0 {
			row["hc_book"], row["hc_title"] = strconv.Itoa(book.ID), book.Title
		}
		status := statuses[book.ID]
		row["hc_status"] = statusName(status)
		switch {
		case d.Outcome != match.Automatic:
		case status == hardcover.StatusRead:
			row["note"] = "already read on Hardcover"
		case b.finished == "":
			row["note"] = "no date on the Kobo, yes marks it read with no date"
		default:
			row["write"] = "yes"
			yes++
		}
		var rec []string
		for _, h := range header {
			rec = append(rec, row[h])
		}
		out.Write(rec)
	}
	out.Flush()
	log.Printf("%d finished on the Kobo, %d marked yes", len(books), yes)
	return out.Error()
}

func statusName(id int) string {
	switch id {
	case 0:
		return "not on a shelf"
	case 1:
		return "want to read"
	case 2:
		return "currently reading"
	case hardcover.StatusRead:
		return "read"
	}
	return strconv.Itoa(id)
}

// write sends each yes row through Reconcile, which only marks a book read if
// Hardcover doesn't have it as read, so running it twice adds nothing.
func write(ctx context.Context, client *hardcover.Client, planPath string) error {
	f, err := os.Open(planPath)
	if err != nil {
		return err
	}
	defer f.Close()
	in := csv.NewReader(f)
	first, err := in.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range first {
		col[h] = i
	}
	for _, h := range header {
		if _, ok := col[h]; !ok {
			return fmt.Errorf("plan has no %s column", h)
		}
	}
	for {
		rec, err := in.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if rec[col["write"]] != "yes" {
			continue
		}
		id, err := strconv.Atoi(rec[col["hc_book"]])
		if err != nil {
			return fmt.Errorf("%s: a yes row needs hc_book", rec[col["kobo_title"]])
		}
		var steps []string
		if rec[col["finished"]] == "" {
			steps, err = client.MarkReadUndated(ctx, id)
		} else {
			want := hardcover.Want{BookID: id, ISBN: rec[col["isbn"]], Read: true, Percent: 100,
				Date: rec[col["finished"]], Started: rec[col["started"]]}
			steps, err = client.Reconcile(ctx, want, false)
		}
		fmt.Printf("%s -> %d: %s\n", rec[col["kobo_title"]], id, strings.Join(steps, "; "))
		if err != nil {
			return fmt.Errorf("%s: %w", rec[col["kobo_title"]], err)
		}
	}
}
