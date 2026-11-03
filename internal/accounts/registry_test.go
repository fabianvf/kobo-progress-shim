package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

type memStore map[string][]byte

func (m memStore) SealedAccounts(context.Context) (map[string][]byte, error) { return m, nil }
func (m memStore) PutSealedAccount(_ context.Context, email string, sealed []byte) error {
	m[email] = sealed
	return nil
}

func newKey(t *testing.T) string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestSavedAccountsAreSealedAndReopen(t *testing.T) {
	ctx := context.Background()
	key, store := newKey(t), memStore{}
	fixed := []Account{{Name: "fabian", KoboToken: "tok-f", HardcoverToken: "hc-f"}}
	r, err := NewRegistry(ctx, fixed, store, key)
	if err != nil {
		t.Fatal(err)
	}
	brother := Account{Name: "b@example.com", Email: "b@example.com", KoboToken: "tok-b", HardcoverToken: "hc-sekrit", CWAUser: "b", CWAPassword: "pw-sekrit"}
	if err := r.Save(ctx, brother); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store["b@example.com"], []byte("sekrit")) {
		t.Fatal("token stored in the clear")
	}
	if err := r.Save(ctx, Account{Email: "x@example.com", KoboToken: "tok-f", HardcoverToken: "x"}); err == nil {
		t.Fatal("setup took over a configured account's Kobo")
	}

	again, err := NewRegistry(ctx, fixed, store, key)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.ByKey(Key("tok-b"))
	if !ok || got.CWAPassword != "pw-sekrit" || got.Email != "b@example.com" {
		t.Fatalf("reopened = %+v, %v", got, ok)
	}
	if _, ok := again.ByKey(Key("stranger")); ok {
		t.Error("a stranger's Kobo found an account")
	}
	if _, err := NewRegistry(ctx, fixed, store, newKey(t)); err == nil {
		t.Error("opened with the wrong key")
	}
}

func TestNoKeyMeansNoSetup(t *testing.T) {
	r, err := NewRegistry(context.Background(), []Account{{HardcoverToken: "hc"}}, memStore{}, "")
	if err != nil || r.SetupEnabled() {
		t.Fatalf("enabled %v, err %v", r.SetupEnabled(), err)
	}
	if a, ok := r.ByKey(Key("anything")); !ok || a.Key() != Any {
		t.Fatalf("the Any account didn't take the device: %+v", a)
	}
}
