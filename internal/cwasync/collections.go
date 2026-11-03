package cwasync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/hardcover"
	"github.com/fabianvf/kobo-progress-shim/internal/store"
)

// keyAll is the collection holding every book on any of the user's shelves and
// lists. For an account whose Kobo syncs only shelves, it's what reaches the
// device, so a book anywhere on Hardcover is on the Kobo.
const keyAll = "all"

// dryTag marks a collection dry run only described, so it's described once.
const dryTag = "dry run"

var statusCollections = []struct {
	status int
	name   string
}{
	{hardcover.StatusWantToRead, "Want to Read"},
	{statusReadingID, "Currently Reading"},
	{hardcover.StatusRead, "Read"},
}

const statusReadingID = 2

type wantCollection struct {
	key, name string
	items     []string
}

// desired is the collections Hardcover implies, holding only books CWA has.
// Names are made unique, since CWA returns an existing shelf by name and two
// lists with one name would otherwise share it.
func desired(lib hardcover.Library, byHardcover map[int][]string) []wantCollection {
	uuids := func(ids []int) []string {
		var out []string
		for _, id := range ids {
			out = append(out, byHardcover[id]...)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	var everything []int
	for id := range lib.Status {
		everything = append(everything, id)
	}
	out := []wantCollection{{key: keyAll, name: "Hardcover"}}
	for _, sc := range statusCollections {
		var ids []int
		for id, status := range lib.Status {
			if status == sc.status {
				ids = append(ids, id)
			}
		}
		out = append(out, wantCollection{key: "status:" + strconv.Itoa(sc.status), name: sc.name, items: uuids(ids)})
	}
	for _, l := range lib.Lists {
		everything = append(everything, l.BookIDs...)
		out = append(out, wantCollection{key: "list:" + strconv.Itoa(l.ID), name: l.Name, items: uuids(l.BookIDs)})
	}
	out[0].items = uuids(everything)
	used := map[string]bool{}
	for i := range out {
		name := out[i].name
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s (%d)", out[i].name, n)
		}
		used[name] = true
		out[i].name = name
	}
	return out
}

// collections keeps one Kobo collection per Hardcover shelf and list. It only
// changes what it added itself, so a book someone adds by hand on the Kobo stays,
// and a collection deleted on the Kobo is made again on the next run.
func (s *Syncer) collections(ctx context.Context, a Account, lib hardcover.Library, byHardcover map[int][]string) error {
	have, err := s.books.Collections(ctx, a.Key)
	if err != nil {
		return err
	}
	saved := map[string]store.Collection{}
	for _, c := range have {
		saved[c.Key] = c
	}
	for _, want := range desired(lib, byHardcover) {
		c, ok := saved[want.key]
		delete(saved, want.key)
		if (!ok || c.TagID == "" || c.TagID == dryTag) && len(want.items) == 0 {
			continue // an empty collection isn't made until it has a book
		}
		if err := s.keep(ctx, a, c, ok, want); err != nil {
			return err
		}
	}
	for _, gone := range saved { // a list deleted on Hardcover
		if !s.dryRun && gone.TagID != "" && gone.TagID != dryTag {
			if err := s.cwa.DeleteTag(ctx, a.KoboToken, gone.TagID); err != nil && !errors.Is(err, ErrGone) {
				return err
			}
		}
		if err := s.books.DeleteCollection(ctx, a.Key, gone.Key); err != nil {
			return err
		}
		s.note(ctx, a, "collection "+gone.Name+" removed, gone from Hardcover")
	}
	return nil
}

func (s *Syncer) keep(ctx context.Context, a Account, c store.Collection, ok bool, want wantCollection) error {
	if s.dryRun {
		if ok {
			return nil
		}
		s.note(ctx, a, fmt.Sprintf("would make collection %s with %d books", want.name, len(want.items)))
		return s.books.PutCollection(ctx, a.Key, store.Collection{Key: want.key, Name: want.name, TagID: dryTag})
	}
	if !ok || c.TagID == "" || c.TagID == dryTag {
		return s.create(ctx, a, want)
	}
	if c.Name != want.name {
		if err := s.cwa.RenameTag(ctx, a.KoboToken, c.TagID, want.name); errors.Is(err, ErrGone) {
			return s.create(ctx, a, want)
		} else if err != nil {
			return err
		}
		c.Name = want.name
	}
	add, remove := diff(c.Items, want.items)
	if len(add) == 0 && len(remove) == 0 && c.Name == want.name {
		return s.books.PutCollection(ctx, a.Key, c)
	}
	if len(add) > 0 {
		if err := s.cwa.AddToTag(ctx, a.KoboToken, c.TagID, add); errors.Is(err, ErrGone) {
			return s.create(ctx, a, want)
		} else if err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		if err := s.cwa.RemoveFromTag(ctx, a.KoboToken, c.TagID, remove); errors.Is(err, ErrGone) {
			return s.create(ctx, a, want)
		} else if err != nil {
			return err
		}
	}
	c.Items = want.items
	s.note(ctx, a, fmt.Sprintf("collection %s: %d added, %d removed", want.name, len(add), len(remove)))
	return s.books.PutCollection(ctx, a.Key, c)
}

// create makes the collection. For an account the shim set up, the shelf is made
// through the web first so it's marked to sync, and the Kobo API then returns
// it by name.
func (s *Syncer) create(ctx context.Context, a Account, want wantCollection) error {
	synced := false
	if a.CWAUser != "" && s.shelves != nil {
		if err := s.shelves.CreateSyncedShelf(ctx, a.CWAUser, a.CWAPassword, want.name); err != nil {
			return err
		}
		synced = true
	}
	id, err := s.cwa.CreateTag(ctx, a.KoboToken, want.name, want.items)
	if err != nil {
		return err
	}
	s.note(ctx, a, fmt.Sprintf("collection %s made with %d books", want.name, len(want.items)))
	return s.books.PutCollection(ctx, a.Key, store.Collection{Key: want.key, Name: want.name, TagID: id, Items: want.items, Synced: synced})
}

func diff(old, want []string) (add, remove []string) {
	for _, u := range want {
		if !slices.Contains(old, u) {
			add = append(add, u)
		}
	}
	for _, u := range old {
		if !slices.Contains(want, u) {
			remove = append(remove, u)
		}
	}
	return add, remove
}

func (s *Syncer) note(ctx context.Context, a Account, detail string) {
	log.Printf("cwa sync %s: %s", a.Key, detail)
	if err := s.books.AddEvent(ctx, store.Event{Time: time.Now(), User: a.Key, Detail: detail}); err != nil {
		log.Printf("cwa sync %s: %v", a.Key, err)
	}
}
