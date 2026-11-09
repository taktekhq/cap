package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseScalar(t *testing.T) {
	cases := map[string]string{
		`plain value`:                      "plain value",
		`plain # comment`:                  "plain",
		`'it''s quoted'`:                   "it's quoted",
		`"a \"b\" \\ c"`:                   `a "b" \ c`,
		`"tab\there"`:                      "tab\there",
		`"é"`:                              "é",
		`'cd ~/x && bin/get k >/dev/null'`: "cd ~/x && bin/get k >/dev/null",
		``:                                 "",
	}
	for in, want := range cases {
		if got := parseScalar(in); got != want {
			t.Errorf("parseScalar(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteScalarRoundTrip(t *testing.T) {
	for _, s := range []string{
		"verified", "2026-11-09", "exit 1: curl: (22) error 401", `say "hi"`, `back\slash`,
		"yes", "- dash", "# hash", "", "a: b", "trailing ",
	} {
		q := quoteScalar(s)
		if got := parseScalar(q); got != s {
			t.Errorf("round trip %q -> %s -> %q", s, q, got)
		}
	}
	if q := quoteScalar("verified"); q != "verified" {
		t.Errorf("plain word should stay unquoted, got %s", q)
	}
}

func writePage(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "b.md", "---\nid: bee\nstatus: unverified\ncheck: 'true'\ncheck_on: mac\n---\nbody\n")
	writePage(t, dir, "a.md", "---\nkind: cli\ncheck: none\n---\n")
	writePage(t, dir, "INDEX.md", "# no front matter\n")
	writePage(t, dir, "_domain.md", "---\ndomain: tools\n---\n# a list, not a capability\n")
	writePage(t, dir, "notes.txt", "---\nid: ignored\n---\n")
	pages, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want 2", len(pages))
	}
	if pages[0].ID != "a" || !pages[0].IsDoc() {
		t.Errorf("page a: id %q doc %v", pages[0].ID, pages[0].IsDoc())
	}
	if pages[1].ID != "bee" || pages[1].Check() != "true" || pages[1].Host() != "mac" {
		t.Errorf("page bee: %+v", pages[1].Fields)
	}
}

func TestLoadDirDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "a.md", "---\nid: same\n---\n")
	writePage(t, dir, "b.md", "---\nid: same\n---\n")
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("want an error for a duplicate id")
	}
}

func TestHostLocalNames(t *testing.T) {
	for _, h := range []string{"", "local", "LOCAL", "localhost"} {
		if got := normalizeHost(h); got != "" {
			t.Errorf("normalizeHost(%q) = %q, want local", h, got)
		}
	}
	if name, err := os.Hostname(); err == nil {
		if got := normalizeHost(name); got != "" {
			t.Errorf("own hostname %q should be local, got %q", name, got)
		}
	}
	if got := normalizeHost("mac"); got != "mac" {
		t.Errorf("normalizeHost(mac) = %q", got)
	}
}

const sample = `---
id: thing
kind: cli
description: "Load when: a thing"
status: unverified
last_verified: 2026-10-22
probe: "old prose"
aliases: ["a", "b"]
check: 'command -v thing'
---
## Use
Body stays exactly the same.
status: this line is in the body, not front matter
`

func TestEditFrontMatterReplace(t *testing.T) {
	out, changed, err := EditFrontMatter([]byte(sample), []Edit{
		{Key: "status", Value: "verified"},
		{Key: "last_verified", Value: "2026-11-09"},
		{Key: "last_checked", Value: "2026-11-09"},
		{Key: "last_error", Delete: true},
	})
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	want := strings.Replace(sample, "status: unverified\nlast_verified: 2026-10-22\n",
		"status: verified\nlast_verified: 2026-11-09\nlast_checked: 2026-11-09\n", 1)
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestEditFrontMatterInsertAndDelete(t *testing.T) {
	in := "---\nid: x\nstatus: verified\nkind: api\n---\nbody\n"
	out, _, err := EditFrontMatter([]byte(in), []Edit{
		{Key: "status", Value: "failing"},
		{Key: "last_checked", Value: "2026-11-09"},
		{Key: "last_error", Value: quoteScalar("exit 22: curl: (22) 401")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nid: x\nstatus: failing\nlast_checked: 2026-11-09\nlast_error: \"exit 22: curl: (22) 401\"\nkind: api\n---\nbody\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	// And back to verified: last_error goes, last_verified lands after status.
	out2, _, err := EditFrontMatter(out, []Edit{
		{Key: "status", Value: "verified"},
		{Key: "last_verified", Value: "2026-11-10"},
		{Key: "last_checked", Value: "2026-11-10"},
		{Key: "last_error", Delete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want2 := "---\nid: x\nstatus: verified\nlast_verified: 2026-11-10\nlast_checked: 2026-11-10\nkind: api\n---\nbody\n"
	if string(out2) != want2 {
		t.Errorf("got:\n%s\nwant:\n%s", out2, want2)
	}
}

func TestEditFrontMatterNoChange(t *testing.T) {
	in := "---\nid: x\nstatus: doc\n---\nbody\n"
	out, changed, err := EditFrontMatter([]byte(in), []Edit{{Key: "status", Value: "doc"}, {Key: "last_error", Delete: true}})
	if err != nil || changed || string(out) != in {
		t.Fatalf("want no change, got changed=%v err=%v", changed, err)
	}
}

func TestEditFrontMatterNoStatusAppends(t *testing.T) {
	in := "---\nid: x\n---\n"
	out, _, err := EditFrontMatter([]byte(in), []Edit{{Key: "status", Value: "doc"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "---\nid: x\nstatus: doc\n---\n" {
		t.Errorf("got %q", out)
	}
}

func TestEditFrontMatterMissing(t *testing.T) {
	if _, _, err := EditFrontMatter([]byte("# no front matter\n"), []Edit{{Key: "status", Value: "doc"}}); err == nil {
		t.Fatal("want an error")
	}
}
