package main

import "testing"

func TestDesktopExecQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/claude-account-switcher": "/usr/local/bin/claude-account-switcher",
		"/home/a b/switcher":                     `"/home/a b/switcher"`,
		"/home/a/100%/s":                         `"/home/a/100%%/s"`,
		`/home/a/"q"/s`:                          `"/home/a/\\"q\\"/s"`,
	}
	for in, want := range cases {
		if got := desktopExecQuote(in); got != want {
			t.Errorf("desktopExecQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSystemLanguages(t *testing.T) {
	t.Setenv("LANGUAGE", "zh_CN:en")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "C")
	t.Setenv("LANG", "en_US.UTF-8")
	got := systemLanguages()
	want := []string{"zh_CN", "en", "en_US.UTF-8"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
