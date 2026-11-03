package cwasync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session is a logged-in CWA web session. Only what the Kobo API can't do goes
// through it: marking a shelf to sync, which only its owner can do, and the
// admin steps that set up a new user. Its errors never carry form values.
type Session struct {
	base *url.URL
	http *http.Client
}

var csrfField = regexp.MustCompile(`name="csrf_token"[^>]*value="([^"]+)"`)

// Login starts a session. CWA answers a good login with a redirect and a bad
// one with the login page again.
func Login(ctx context.Context, base *url.URL, user, password string) (*Session, error) {
	s := &Session{base: base, http: &http.Client{
		Jar:           &oneHostJar{cookies: map[string]*http.Cookie{}},
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	status, err := s.post(ctx, "login", "login", url.Values{"username": {user}, "password": {password}, "remember_me": {"on"}, "next": {"/"}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusFound {
		return nil, errors.New("cwa: login refused")
	}
	return s, nil
}

// CreateSyncedShelf makes a private shelf marked to sync to the Kobo. Its uuid
// comes from the Kobo API afterwards, which returns an existing shelf by name.
func (s *Session) CreateSyncedShelf(ctx context.Context, name string) error {
	status, err := s.post(ctx, "shelf/create", "shelf/create", url.Values{"title": {name}, "kobo_sync": {"on"}})
	if err != nil {
		return err
	}
	if status != http.StatusFound {
		return fmt.Errorf("cwa: creating shelf: status %d", status)
	}
	return nil
}

var shelfLink = regexp.MustCompile(`href="[^"]*/shelf/(\d+)" title="([^"]*) \(\d+\)"`)

// MarkShelfSynced turns on "sync to Kobo" for the session user's shelf of this
// name, for a shelf the Kobo API made, which starts without it.
func (s *Session) MarkShelfSynced(ctx context.Context, name string) error {
	data, status, err := s.get(ctx, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("cwa: reading shelves: status %d", status)
	}
	for _, m := range shelfLink.FindAllSubmatch(data, -1) {
		if html.UnescapeString(string(m[2])) != name {
			continue
		}
		edit := "shelf/edit/" + string(m[1])
		code, err := s.post(ctx, edit, edit, url.Values{"title": {name}, "kobo_sync": {"on"}})
		if err != nil {
			return err
		}
		if code != http.StatusFound {
			return fmt.Errorf("cwa: marking shelf to sync: status %d", code)
		}
		return nil
	}
	return fmt.Errorf("cwa: no shelf named %q", name)
}

// NewUser is what the robot needs to set up someone's CWA account.
type NewUser struct {
	Name     string
	Email    string
	Password string
}

// CreateUser makes a CWA user who can download and edit their own shelves, with
// only shelves syncing to their Kobo, and returns their id and Kobo token.
func (s *Session) CreateUser(ctx context.Context, u NewUser) (int, string, error) {
	form := url.Values{
		"name": {u.Name}, "email": {u.Email}, "password": {u.Password},
		"download_role": {"on"}, "viewer_role": {"on"}, "edit_shelf_role": {"on"},
		"kobo_only_shelves_sync": {"on"}, "default_language": {"all"}, "locale": {"en"},
	}
	status, body, err := s.postBody(ctx, "admin/user/new", "admin/user/new", form)
	if err != nil {
		return 0, "", err
	}
	// CWA answers 200 either way, so whether the user now exists is the answer.
	id, err := s.userID(ctx, u.Name)
	if err != nil {
		if status == http.StatusOK || status == http.StatusFound {
			return 0, "", fmt.Errorf("cwa: creating user refused: %s", refusal(body))
		}
		return 0, "", fmt.Errorf("cwa: creating user: status %d", status)
	}
	token, err := s.KoboToken(ctx, id)
	if err != nil {
		// A user with no usable token is no account at all, and would make the
		// next try fail on a taken name, so it goes.
		if delErr := s.DeleteUser(ctx, id); delErr != nil {
			return 0, "", fmt.Errorf("%w, and removing the half-made user failed: %v", err, delErr)
		}
		return 0, "", err
	}
	return id, token, nil
}

// UserID finds a CWA user by name.
func (s *Session) UserID(ctx context.Context, name string) (int, error) { return s.userID(ctx, name) }

func (s *Session) userID(ctx context.Context, name string) (int, error) {
	data, status, err := s.get(ctx, "ajax/listusers?limit=1000&offset=0&search="+url.QueryEscape(name))
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("cwa: listing users: status %d", status)
	}
	var out struct {
		Rows []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, errors.New("cwa: listing users: unexpected response")
	}
	for _, r := range out.Rows {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, errors.New("cwa: new user not found")
}

var koboToken = regexp.MustCompile(`/kobo/([0-9a-f]{32})`)

// KoboToken returns a user's Kobo sync token, making one if they have none.
func (s *Session) KoboToken(ctx context.Context, userID int) (string, error) {
	data, status, err := s.get(ctx, "kobo_auth/generate_auth_token/"+strconv.Itoa(userID))
	if err != nil {
		return "", err
	}
	m := koboToken.FindSubmatch(data)
	if status != http.StatusOK || m == nil {
		return "", errors.New("cwa: no Kobo token on the setup page")
	}
	return string(m[1]), nil
}

// flashError is CWA's error message on a refused form. It's CWA's own text, so
// it carries no form values.
var flashError = regexp.MustCompile(`(?s)alert-danger[^>]*>(.*?)</div>`)

var tags = regexp.MustCompile(`<[^>]+>`)

// post sends a form with the CSRF token from page, and returns the status.
func (s *Session) post(ctx context.Context, page, path string, form url.Values) (int, error) {
	status, _, err := s.postBody(ctx, page, path, form)
	return status, err
}

func (s *Session) postBody(ctx context.Context, page, path string, form url.Values) (int, []byte, error) {
	data, _, err := s.get(ctx, page)
	if err != nil {
		return 0, nil, err
	}
	m := csrfField.FindSubmatch(data)
	if m == nil {
		return 0, nil, fmt.Errorf("cwa: no form on %s", page)
	}
	form.Set("csrf_token", string(m[1]))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.JoinPath(path).String(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRFToken", string(m[1]))
	req.Host = s.host()
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cwa: %s: %w", path, unwrapURL(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, nil
}

func refusal(body []byte) string {
	m := flashError.FindSubmatch(body)
	if m == nil {
		return "no reason given"
	}
	return strings.Join(strings.Fields(tags.ReplaceAllString(string(m[1]), " ")), " ")
}

func (s *Session) get(ctx context.Context, path string) ([]byte, int, error) {
	target, err := s.base.Parse(path)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Host = s.host()
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cwa: %s: %w", strings.SplitN(path, "?", 2)[0], unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, err
}

// localName stands in for a loopback host. CWA hides a user's Kobo token from a
// request whose Host is localhost (cps/kobo_auth.py:76-90), which is how the shim
// reaches it inside the pod. The name only reaches CWA's Host check, nothing else.
const localName = "cwa.internal"

func (s *Session) host() string {
	h := s.base.Hostname()
	if h == "localhost" || strings.HasPrefix(h, "127.") || h == "::1" {
		if port := s.base.Port(); port != "" {
			return localName + ":" + port
		}
		return localName
	}
	return s.base.Host
}

func unwrapURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// DeleteUser removes a CWA user with their shelves and read status, for undoing
// a setup. CWA refuses to delete the last admin.
func (s *Session) DeleteUser(ctx context.Context, id int) error {
	status, err := s.post(ctx, "admin/view", "ajax/deleteuser", url.Values{"userid": {strconv.Itoa(id)}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("cwa: deleting user: status %d", status)
	}
	return nil
}

// oneHostJar keeps a session's cookies for the one CWA it talks to, and sends them
// back even when CWA marks them Secure. CWA does that when it enforces HTTPS or
// OAuth is on, while the shim reaches it over plain HTTP inside the pod, where a
// standard jar would drop them and every login would quietly fail.
type oneHostJar struct {
	mu      sync.Mutex
	cookies map[string]*http.Cookie
}

func (j *oneHostJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		if c.MaxAge < 0 {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
	}
}

func (j *oneHostJar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, c := range j.cookies {
		out = append(out, c)
	}
	return out
}

// DeleteShelf removes the session user's shelf of this name.
func (s *Session) DeleteShelf(ctx context.Context, name string) error {
	data, _, err := s.get(ctx, "")
	if err != nil {
		return err
	}
	for _, m := range shelfLink.FindAllSubmatch(data, -1) {
		if html.UnescapeString(string(m[2])) == name {
			code, err := s.post(ctx, "shelf/edit/"+string(m[1]), "shelf/delete/"+string(m[1]), url.Values{})
			if err != nil {
				return err
			}
			if code != http.StatusFound && code != http.StatusOK {
				return fmt.Errorf("cwa: deleting shelf: status %d", code)
			}
			return nil
		}
	}
	return fmt.Errorf("cwa: no shelf named %q", name)
}
