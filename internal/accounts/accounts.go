// Package accounts says which Hardcover account a Kobo's traffic belongs to.
// Every Kobo request carries its user's CWA sync token in the path, so that token
// is the identity, and a key derived from it is what the store and logs see.
package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Any is the key of an account with no Kobo token, which takes every device. It
// keeps a single-user setup working before its token is configured.
const Any = "*"

type Account struct {
	Name           string `json:"name"`
	KoboToken      string `json:"kobo_token"`
	HardcoverToken string `json:"hardcover_token"`
}

// Key identifies the account in the store and logs without exposing its token.
func (a Account) Key() string {
	if a.KoboToken == "" {
		return Any
	}
	return Key(a.KoboToken)
}

// Key is a short hash of a CWA sync token. The token is a secret, the key isn't.
func Key(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:12])
}

// Load reads ACCOUNTS_FILE, a JSON list of accounts. Without one it falls back to
// HARDCOVER_API_KEY and an optional KOBO_SYNC_TOKEN as a single account.
func Load() ([]Account, error) {
	if path := os.Getenv("ACCOUNTS_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var list []Account
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, errors.New("accounts: ACCOUNTS_FILE isn't a JSON list of accounts")
		}
		return check(list)
	}
	token := os.Getenv("HARDCOVER_API_KEY")
	if token == "" {
		return nil, nil
	}
	return check([]Account{{Name: "default", KoboToken: os.Getenv("KOBO_SYNC_TOKEN"), HardcoverToken: token}})
}

// check refuses a list where one device could belong to two accounts, because
// its progress would then go to whichever was found first.
func check(list []Account) ([]Account, error) {
	seen := map[string]bool{}
	for i := range list {
		a := &list[i]
		a.KoboToken, a.HardcoverToken = strings.TrimSpace(a.KoboToken), strings.TrimSpace(a.HardcoverToken)
		if a.HardcoverToken == "" {
			return nil, fmt.Errorf("accounts: %q has no Hardcover token", a.Name)
		}
		if seen[a.Key()] || (a.Key() == Any && len(list) > 1) {
			return nil, fmt.Errorf("accounts: %q shares a Kobo with another account", a.Name)
		}
		seen[a.Key()] = true
	}
	return list, nil
}
