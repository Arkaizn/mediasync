package redact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := map[string]string{
		"https://jf.example/Items/abc/Images/Primary?tag=x&api_key=SECRET": "https://jf.example/Items/abc/Images/Primary?tag=x",
		"https://user:pass@host/path":                                      "https://host/path",
		"file:///home/u/cover.jpg":                                         "file:///home/u/cover.jpg",
		"https://h/a?X-Emby-Token=S&Foo=1":                                 "https://h/a?Foo=1",
		"":                                                                 "",
	}
	for in, want := range cases {
		got, ok := URL(in)
		if !ok || got != want {
			t.Errorf("URL(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := URL("http://[::1"); ok {
		t.Error("expected parse failure")
	}
}

func TestString(t *testing.T) {
	s := String(`request failed: Authorization: MediaBrowser Token="abc123" url?api_key=zzz&x=1`)
	if strings.Contains(s, "abc123") || strings.Contains(s, "zzz") {
		t.Fatalf("not redacted: %s", s)
	}
}

func TestSecret(t *testing.T) {
	sec := Secret("hunter2")
	var b strings.Builder
	fmt.Fprintf(&b, "%s %v %+v %#v", sec, sec, sec, sec)
	j, _ := json.Marshal(map[string]any{"t": sec})
	b.Write(j)
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", "tok", sec)
	slog.New(slog.NewJSONHandler(&b, nil)).Info("x", "tok", sec)
	if strings.Contains(b.String(), "hunter2") {
		t.Fatalf("secret leaked: %s", b.String())
	}
	if sec.Reveal() != "hunter2" {
		t.Fatal("reveal")
	}
}
