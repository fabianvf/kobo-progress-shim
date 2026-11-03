package setup

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Access checks the token Cloudflare Access puts on each request it lets through,
// and returns the signed-in email. The setup page trusts nothing else, because on
// the LAN it can be reached without passing Access, so a bare header proves nothing.
type Access struct {
	team     string // https://<team>.cloudflareaccess.com
	audience string // the Access application's AUD tag
	http     *http.Client
	now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func NewAccess(team, audience string) *Access {
	return &Access{team: strings.TrimSuffix(team, "/"), audience: audience, http: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
}

var errNoIdentity = errors.New("setup: not signed in through Cloudflare Access")

func (a *Access) Email(r *http.Request) (string, error) {
	token := r.Header.Get("Cf-Access-Jwt-Assertion")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errNoIdentity
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil || header.Alg != "RS256" {
		return "", errNoIdentity
	}
	key, err := a.key(r.Context(), header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errNoIdentity
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return "", errNoIdentity
	}
	var claims struct {
		Aud   audience `json:"aud"`
		Exp   int64    `json:"exp"`
		Iss   string   `json:"iss"`
		Email string   `json:"email"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return "", errNoIdentity
	}
	if !slices.Contains(claims.Aud, a.audience) || claims.Iss != a.team || a.now().Unix() >= claims.Exp || claims.Email == "" {
		return "", errNoIdentity
	}
	return strings.ToLower(claims.Email), nil
}

// audience is a JWT aud, which may be one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	err := json.Unmarshal(data, &many)
	*a = many
	return err
}

func decodePart(part string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// key returns the signing key by id, refetching the team's keys when one is
// unknown, at most once a minute, since Access rotates them.
func (a *Access) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.keys[kid]; ok {
		return k, nil
	}
	if a.now().Sub(a.fetched) < time.Minute {
		return nil, errNoIdentity
	}
	a.fetched = a.now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.team+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, errors.New("setup: Access keys unreadable")
	}
	a.keys = map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if k.Kty != "RSA" || errN != nil || errE != nil {
			continue
		}
		a.keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if k, ok := a.keys[kid]; ok {
		return k, nil
	}
	return nil, errNoIdentity
}
