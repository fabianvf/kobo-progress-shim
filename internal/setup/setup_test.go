package setup

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fabianvf/kobo-progress-shim/internal/accounts"
)

type fixedIdentity string

func (f fixedIdentity) Email(*http.Request) (string, error) {
	if f == "" {
		return "", errNoIdentity
	}
	return string(f), nil
}

type memStore map[string][]byte

func (m memStore) SealedAccounts(context.Context) (map[string][]byte, error) { return m, nil }
func (m memStore) PutSealedAccount(_ context.Context, email string, sealed []byte) error {
	m[email] = sealed
	return nil
}

type fakeCWA struct {
	created []string
	taken   map[string]bool
}

func (f *fakeCWA) CreateUser(_ context.Context, name, email, password string) (string, error) {
	if len(password) < 20 {
		return "", errors.New("weak password")
	}
	if f.taken[name] {
		return "", errors.New("cwa: creating user refused: This username is already taken")
	}
	f.created = append(f.created, name+"/"+email)
	return "0123456789abcdef0123456789abcdef", nil
}

func registry(t *testing.T) *accounts.Registry {
	raw := make([]byte, 32)
	rand.Read(raw)
	r, err := accounts.NewRegistry(context.Background(), []accounts.Account{{Name: "fabian", KoboToken: "f", HardcoverToken: "h"}}, memStore{}, base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type prefixed struct {
	*httptest.Server
	URL string
}

func server(t *testing.T, id Identity, reg *accounts.Registry, cwa CWA) *prefixed {
	hc := func(_ context.Context, token string) (string, error) {
		if token != "good-key" {
			return "", errors.New("401")
		}
		return "brother", nil
	}
	seen := func(context.Context, string) (time.Time, error) { return time.Time{}, nil }
	mux := http.NewServeMux()
	New(id, reg, hc, cwa, seen, "https://library.example").Register(mux, "/setup")
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return &prefixed{s, s.URL + "/setup/"}
}

func body(t *testing.T, call func() (*http.Response, error)) string {
	t.Helper()
	resp, err := call()
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

func TestJoiningMakesOneAccount(t *testing.T) {
	reg, cwa := registry(t), &fakeCWA{}
	s := server(t, fixedIdentity("Brother@Example.com"), reg, cwa)
	if page := body(t, func() (*http.Response, error) { return http.Get(s.URL) }); !strings.Contains(page, "Connect Hardcover") || !strings.Contains(page, newKeyURL) {
		t.Fatalf("page = %s", page)
	}
	if page := body(t, func() (*http.Response, error) { return http.PostForm(s.URL, url.Values{"hardcover_key": {"bad"}}) }); !strings.Contains(page, "accept that key") || len(cwa.created) != 0 {
		t.Fatalf("bad key: created %v, page %s", cwa.created, page)
	}
	page := body(t, func() (*http.Response, error) {
		return http.PostForm(s.URL, url.Values{"hardcover_key": {"Bearer good-key"}})
	})
	if !strings.Contains(page, "api_endpoint=https://library.example/kobo/0123456789abcdef0123456789abcdef") || !strings.Contains(page, "as brother") {
		t.Fatalf("page = %s", page)
	}
	body(t, func() (*http.Response, error) { return http.PostForm(s.URL, url.Values{"hardcover_key": {"good-key"}}) })
	if len(cwa.created) != 1 || cwa.created[0] != "kobo-brother/Brother@Example.com" {
		t.Fatalf("created = %v", cwa.created)
	}
	if page := body(t, func() (*http.Response, error) { return http.Get(s.URL) }); !strings.Contains(page, "kobo-brother") {
		t.Fatalf("CWA login not shown: %s", page)
	}
	a, ok := reg.ByEmail("Brother@Example.com")
	if !ok || a.HardcoverToken != "good-key" || a.CWAPassword == "" {
		t.Fatalf("account = %+v", a)
	}
}

func TestBarePathRedirects(t *testing.T) {
	s := server(t, fixedIdentity("a@example.com"), registry(t), &fakeCWA{})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(strings.TrimSuffix(s.URL, "/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/setup/" {
		t.Fatalf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestNoIdentityNoPage(t *testing.T) {
	s := server(t, fixedIdentity(""), registry(t), &fakeCWA{})
	resp, err := http.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func signed(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	enc := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": "k1"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(head))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestAccessChecksTheToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer certs.Close()
	a := NewAccess(certs.URL, "aud-1")
	good := map[string]any{"aud": []string{"aud-1"}, "iss": certs.URL, "exp": time.Now().Add(time.Hour).Unix(), "email": "B@Example.com"}
	check := func(token string) (string, error) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
		return a.Email(r)
	}
	if email, err := check(signed(t, key, good)); err != nil || email != "b@example.com" {
		t.Fatalf("email %q, err %v", email, err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad := map[string]map[string]any{"other audience": {"aud": "aud-2"}, "expired": {"exp": time.Now().Add(-time.Minute).Unix()}, "other issuer": {"iss": "https://evil"}}
	for name, change := range bad {
		claims := map[string]any{}
		for k, v := range good {
			claims[k] = v
		}
		for k, v := range change {
			claims[k] = v
		}
		if _, err := check(signed(t, key, claims)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := check(signed(t, other, good)); err == nil {
		t.Error("a token signed by another key was accepted")
	}
	if _, err := check(""); err == nil {
		t.Error("no token accepted")
	}
}

func TestTakenNameGetsANumber(t *testing.T) {
	reg, cwa := registry(t), &fakeCWA{taken: map[string]bool{"kobo-brother": true}}
	s := server(t, fixedIdentity("brother@work.example"), reg, cwa)
	body(t, func() (*http.Response, error) { return http.PostForm(s.URL, url.Values{"hardcover_key": {"good-key"}}) })
	if a, ok := reg.ByEmail("brother@work.example"); !ok || a.CWAUser != "kobo-brother-2" {
		t.Fatalf("account = %+v, created %v", a, cwa.created)
	}
}
