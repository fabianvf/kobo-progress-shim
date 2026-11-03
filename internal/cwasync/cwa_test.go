package cwasync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const page1 = `<feed xmlns="http://www.w3.org/2005/Atom">
<link rel="next" href="/opds/new?offset=1"/>
<entry><title>Arm of the Sphinx</title><id>urn:uuid:3170863a-f50b-4453-b7fd-a36abe72d1f1</id><author><name>Josiah Bancroft</name></author></entry>
</feed>`

const page2 = `<feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>Evershore</title><id>urn:uuid:aaaaaaaa-0000-0000-0000-000000000000</id><author><name>Brandon Sanderson</name></author><author><name>Janci Patterson</name></author></entry>
</feed>`

func TestBooksFollowsPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "robot" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("offset") == "1" {
			io.WriteString(w, page2)
			return
		}
		io.WriteString(w, page1)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	books, err := NewCWA(base, "robot", "pw").Books(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 2 || books[0].UUID != "3170863a-f50b-4453-b7fd-a36abe72d1f1" || len(books[1].Authors) != 2 {
		t.Fatalf("books = %+v", books)
	}
}

func TestStateKeepsTheBookmark(t *testing.T) {
	const uuid = "3170863a-f50b-4453-b7fd-a36abe72d1f1"
	var put map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kobo/tok/v1/library/"+uuid+"/state" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&put)
			io.WriteString(w, `{"RequestResult":"Success"}`)
			return
		}
		io.WriteString(w, `[{"EntitlementId":"`+uuid+`","StatusInfo":{"Status":"ReadyToRead"},"CurrentBookmark":{"ProgressPercent":12,"ContentSourceProgressPercent":40}}]`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	c := NewCWA(base, "", "")
	st, err := c.State(context.Background(), "tok", uuid)
	if err != nil || st.Status != statusUnread || *st.Percent != 12 {
		t.Fatalf("state = %+v, err %v", st, err)
	}
	if err := c.SetStatus(context.Background(), "tok", uuid, st, statusFinished); err != nil {
		t.Fatal(err)
	}
	state := put["ReadingStates"].([]any)[0].(map[string]any)
	bookmark := state["CurrentBookmark"].(map[string]any)
	if state["StatusInfo"].(map[string]any)["Status"] != statusFinished || bookmark["ProgressPercent"] != 12.0 || bookmark["ContentSourceProgressPercent"] != 40.0 {
		t.Fatalf("put = %+v", put)
	}
	if _, ok := state["Statistics"]; !ok {
		t.Error("CWA's handler needs a Statistics key")
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://storeapi.kobo.com/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	base, _ := url.Parse(redirect.URL + "/")
	_, err := NewCWA(base, "", "").State(context.Background(), "sekrit", "id")
	if err == nil || strings.Contains(err.Error(), "sekrit") || !strings.Contains(err.Error(), "307") {
		t.Fatalf("err = %v", err)
	}
	closed, _ := url.Parse("http://127.0.0.1:1/")
	_, err = NewCWA(closed, "", "").State(context.Background(), "sekrit", "id")
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionKeepsSecureCookiesOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodGet:
			io.WriteString(w, `<form><input type="hidden" name="csrf_token" value="tok"></form>`)
		case r.URL.Path == "/login":
			if r.FormValue("csrf_token") != "tok" || r.FormValue("password") != "pw" {
				io.WriteString(w, `<div class="alert alert-danger">Wrong Username or Password</div>`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Secure: true, HttpOnly: true})
			http.Redirect(w, r, "/", http.StatusFound)
		case r.URL.Path == "/shelf/create" && r.Method == http.MethodGet:
			if c, err := r.Cookie("session"); err != nil || c.Value != "s1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `<input type="hidden" name="csrf_token" value="tok2">`)
		case r.URL.Path == "/shelf/create":
			if c, err := r.Cookie("session"); err != nil || c.Value != "s1" || r.FormValue("kobo_sync") != "on" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/shelf/9", http.StatusFound)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	if _, err := Login(context.Background(), base, "u", "wrong"); err == nil {
		t.Fatal("a bad login worked")
	}
	s, err := Login(context.Background(), base, "u", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSyncedShelf(context.Background(), "Want to Read"); err != nil {
		t.Fatal(err)
	}
}

func TestMarkShelfSyncedFindsTheShelfByName(t *testing.T) {
	var edited url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, `<li><a href="/shelf/7" title="Want to Read (12)">Want to Read</a></li><li><a href="/shelf/9" title="Fabian&#39;s List (2)">x</a></li>`)
		case r.URL.Path == "/shelf/edit/9" && r.Method == http.MethodGet:
			io.WriteString(w, `<input type="hidden" name="csrf_token" value="tok">`)
		case r.URL.Path == "/shelf/edit/9":
			r.ParseForm()
			edited = r.PostForm
			http.Redirect(w, r, "/shelf/9", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	s := &Session{base: base, http: &http.Client{Jar: &oneHostJar{cookies: map[string]*http.Cookie{}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := s.MarkShelfSynced(context.Background(), "Fabian's List"); err != nil {
		t.Fatal(err)
	}
	if edited.Get("kobo_sync") != "on" || edited.Get("title") != "Fabian's List" || edited.Get("csrf_token") != "tok" {
		t.Fatalf("edited = %v", edited)
	}
	if err := s.MarkShelfSynced(context.Background(), "Missing"); err == nil {
		t.Error("a missing shelf wasn't an error")
	}
}

func TestSessionHidesALoopbackHost(t *testing.T) {
	var hosts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodGet:
			io.WriteString(w, `<input type="hidden" name="csrf_token" value="tok">`)
		case r.URL.Path == "/login":
			http.Redirect(w, r, "/", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/kobo_auth/generate_auth_token/"):
			if strings.HasPrefix(r.Host, "127.") || strings.HasPrefix(r.Host, "localhost") {
				io.WriteString(w, `Please access Calibre-Web Automated from non localhost`)
				return
			}
			io.WriteString(w, `api_endpoint=https://x/kobo/0123456789abcdef0123456789abcdef`)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/") // 127.0.0.1:<port>
	s, err := Login(context.Background(), base, "robot", "pw")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.KoboToken(context.Background(), 6)
	if err != nil || token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("token %q, err %v", token, err)
	}
	for _, h := range hosts {
		if !strings.HasPrefix(h, "cwa.internal:") {
			t.Fatalf("CWA saw Host %q", h)
		}
	}
}
