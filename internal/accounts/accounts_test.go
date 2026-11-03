package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	os.WriteFile(path, []byte(`[{"name":"a","kobo_token":"tok-a","hardcover_token":"hc-a"},{"name":"b","kobo_token":"tok-b","hardcover_token":"hc-b"}]`), 0o600)
	t.Setenv("ACCOUNTS_FILE", path)
	list, err := Load()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	if list[0].Key() != Key("tok-a") || strings.Contains(list[0].Key(), "tok") {
		t.Errorf("key = %q", list[0].Key())
	}
}

func TestLoadRefusesSharedDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	os.WriteFile(path, []byte(`[{"name":"a","kobo_token":"same","hardcover_token":"x"},{"name":"b","kobo_token":"same","hardcover_token":"y"}]`), 0o600)
	t.Setenv("ACCOUNTS_FILE", path)
	if _, err := Load(); err == nil {
		t.Fatal("expected an error")
	}
	os.WriteFile(path, []byte(`[{"name":"a","hardcover_token":"x"},{"name":"b","kobo_token":"t","hardcover_token":"y"}]`), 0o600)
	if _, err := Load(); err == nil {
		t.Fatal("an account taking every device can't share with another")
	}
}

func TestLoadEnvFallback(t *testing.T) {
	t.Setenv("ACCOUNTS_FILE", "")
	t.Setenv("HARDCOVER_API_KEY", "hc")
	t.Setenv("KOBO_SYNC_TOKEN", "")
	list, err := Load()
	if err != nil || len(list) != 1 || list[0].Key() != Any {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	t.Setenv("HARDCOVER_API_KEY", "")
	if list, err := Load(); err != nil || list != nil {
		t.Fatalf("list = %+v, err %v", list, err)
	}
}
