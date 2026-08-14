package opconfig

import (
	"context"
	"strings"
	"testing"
)

func stub(t *testing.T, out string, err error) {
	t.Helper()
	orig := execOP
	execOP = func(context.Context) ([]byte, error) { return []byte(out), err }
	t.Cleanup(func() { execOP = orig })
}

func TestLoadParsesItem(t *testing.T) {
	stub(t, `{"fields": [
		{"label": "credential", "value": "ghp_secret123"},
		{"label": "username", "value": "chrisle"},
		{"label": "notesPlain", "value": ""}
	]}`, nil)
	item, err := Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Token != "ghp_secret123" || item.Login != "chrisle" {
		t.Errorf("item = %+v", item)
	}
}

func TestLoadRequiresUsername(t *testing.T) {
	stub(t, `{"fields": [{"label": "credential", "value": "ghp_x"}]}`, nil)
	if _, err := Load(context.Background()); err == nil || !strings.Contains(err.Error(), "username") {
		t.Errorf("err = %v, want a username error", err)
	}
}
