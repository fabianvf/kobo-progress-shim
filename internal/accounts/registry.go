package accounts

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
)

// Store is where accounts made through setup are kept, sealed.
type Store interface {
	SealedAccounts(ctx context.Context) (map[string][]byte, error)
	PutSealedAccount(ctx context.Context, email string, sealed []byte) error
}

// Registry is every account: those from the file or environment, which a person
// configured, and those made through setup, which are sealed with a key the store
// never holds. It's read on every request and every sync, so new accounts take
// effect without a restart.
type Registry struct {
	mu      sync.RWMutex
	fixed   []Account
	saved   []Account
	store   Store
	aead    cipher.AEAD // nil when no key is configured, and setup is off
	changed func(Account)
}

// NewRegistry opens the saved accounts with key, a base64 32-byte AES key. An
// empty key leaves setup off, and only the fixed accounts exist.
func NewRegistry(ctx context.Context, fixed []Account, store Store, key string) (*Registry, error) {
	r := &Registry{fixed: fixed, store: store}
	if key == "" {
		return r, nil
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("accounts: the key must be 32 bytes, base64")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	if r.aead, err = cipher.NewGCM(block); err != nil {
		return nil, err
	}
	sealed, err := store.SealedAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for email, data := range sealed {
		a, err := r.open(data)
		if err != nil {
			return nil, errors.New("accounts: a saved account doesn't open with this key")
		}
		a.Email = email
		r.saved = append(r.saved, a)
	}
	return r, nil
}

// SetupEnabled reports whether accounts can be added through setup.
func (r *Registry) SetupEnabled() bool { return r.aead != nil }

func (r *Registry) All() []Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append(slices.Clone(r.fixed), r.saved...)
}

// ByKey finds the account a Kobo's traffic belongs to. The Any account takes a
// device no other account claims.
func (r *Registry) ByKey(key string) (Account, bool) {
	var any *Account
	for _, a := range r.All() {
		if a.Key() == key {
			return a, true
		}
		if a.Key() == Any {
			any = &a
		}
	}
	if any != nil {
		return *any, true
	}
	return Account{}, false
}

// ByEmail finds an account by its Access email, ignoring case. configured
// reports whether a person set it up in the file or environment, so setup can
// show it but not change it.
func (r *Registry) ByEmail(email string) (a Account, configured, ok bool) {
	email = strings.ToLower(email)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.fixed {
		if f.Email != "" && f.Email == email {
			return f, true, true
		}
	}
	for _, s := range r.saved {
		if strings.EqualFold(s.Email, email) {
			return s, false, true
		}
	}
	return Account{}, false, false
}

// Save adds or replaces the setup account for a.Email.
func (r *Registry) Save(ctx context.Context, a Account) error {
	if r.aead == nil {
		return errors.New("accounts: setup is off")
	}
	if a.Email == "" || a.KoboToken == "" || a.HardcoverToken == "" {
		return errors.New("accounts: an account needs an email, a Kobo token and a Hardcover token")
	}
	for _, f := range r.fixed {
		if f.Key() == a.Key() || f.Key() == Any || (f.Email != "" && strings.EqualFold(f.Email, a.Email)) {
			return errors.New("accounts: that Kobo or email already belongs to a configured account")
		}
	}
	sealed, err := r.seal(a)
	if err != nil {
		return err
	}
	if err := r.store.PutSealedAccount(ctx, a.Email, sealed); err != nil {
		return err
	}
	r.mu.Lock()
	r.saved = slices.DeleteFunc(r.saved, func(s Account) bool { return s.Email == a.Email })
	r.saved = append(r.saved, a)
	r.mu.Unlock()
	return nil
}

func (r *Registry) seal(a Account) ([]byte, error) {
	plain, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, r.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return r.aead.Seal(nonce, nonce, plain, nil), nil
}

func (r *Registry) open(data []byte) (Account, error) {
	n := r.aead.NonceSize()
	if len(data) < n {
		return Account{}, errors.New("short")
	}
	plain, err := r.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return Account{}, err
	}
	var a Account
	return a, json.Unmarshal(plain, &a)
}
