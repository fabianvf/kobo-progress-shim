---
name: resolve-review
description: Resolve the shim's review queue, the books it couldn't match to Hardcover on its own. Use when the activity page shows "Needs a match", or when asked to clear or look at the review queue. Proposes a Hardcover book for each, shows the picks with reasons, and confirms only what the person approves.
---

# Resolving the review queue

The shim sends a book to review when it can't be sure which Hardcover book it is.
A confirmed match drives public writes to that reader's Hardcover, so a wrong pick
publishes false reading history. Read `docs/kb/arch-matching.md` first: it has the
matching rules and why title and author alone aren't trusted.

Nothing here is confirmed without the person's OK on the specific list.

## 1. Read the queue

The activity page is `https://kobo-progress.apps.playerof.games/`, reachable on
Fabian's LAN only. Each row in "Needs a match" has the title, the reason, the
shim's suggestion, and a form with hidden `user` and `id` fields.

```sh
curl -s https://kobo-progress.apps.playerof.games/ | python3 -I -c '
import sys,re,html
t=sys.stdin.read(); s=re.search(r"Needs a match.*?</table>",t,re.S)
for row in re.findall(r"<tr>(.*?)</tr>",s.group(0) if s else "",re.S)[1:]:
    cells=[html.unescape(re.sub(r"<[^>]+>"," ",c)).strip() for c in re.findall(r"<td>(.*?)</td>",row,re.S)]
    f=dict(re.findall(r"name=\"(user|id)\" value=\"([^\"]*)\"",row))
    print(cells[:3], f)'
```

## 2. Find the candidates

Use Hardcover's API with the key in `.env` (`hardcover_api_key`). Never print it.
Space requests about 1.5s apart: a burst of about 15 got a 429.

For each book, look up every Hardcover book with that exact title by that author,
with what decides between them:

```graphql
query($t: String!, $a: String!) {
  books(where: {title: {_eq: $t}, contributions: {author: {name: {_eq: $a}}}},
        order_by: {users_read_count: desc}) {
    id title release_year users_read_count compilation
    book_series { position series { name } }
  }
}
```

Also check whether the reader has any candidate on a shelf
(`me { user_books(where: {book_id: {_in: [...]}}) { book_id status_id } }`).
The shim already picks a sole shelved candidate itself, so if one is shelved,
say so: it means the shelf has more than one, or the book came in another way.

If Hardcover titles the book differently, as with "Mistborn: The Final Empire",
search for that title too. An ISBN from the CWA file or the store, if known, is
strong evidence: `editions(where: {isbn_13: {_eq: "..."}}) { book_id }`.

## 3. Choose

Pick one only when the evidence clearly points to it:

- The file's ISBN belongs to that Hardcover book.
- Otherwise, among true duplicates (same title, same author), the one with far
  more readers. Tens of times more is clear; a near tie isn't.
- Never a split ("(2 of 3)"), a dramatized or audio adaptation, an omnibus, box
  set or "Books 1-3" bundle, or a different edition's title, unless the book
  itself is one.

When nothing is clear, leave the book in review and say why. Don't guess.

## 4. Show, then confirm

Show the person one line per book: the title, the pick as `id: Hardcover title`,
and the reason (ISBN, reader counts, on their shelf). Wait for their OK on that
list. Approval for one list doesn't cover the next.

Then confirm each approved book on the activity page:

```sh
curl -s -o /dev/null -w '%{http_code}\n' \
  --data-urlencode "user=<user>" --data-urlencode "id=<id>" \
  --data-urlencode "hardcover_id=<pick>" \
  https://kobo-progress.apps.playerof.games/confirm
```

A 303 is success. Read the page again afterwards and check the count went down.
Confirming only fixes the match. The book's latest progress, if it has any, is
written to Hardcover on the shim's next retry, within 5 minutes.
