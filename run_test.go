package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func page(id, check, on string) *Page {
	return &Page{ID: id, Fields: map[string]string{"id": id, "check": check, "check_on": on}}
}

func testRunner() *Runner {
	return &Runner{Timeout: 5 * time.Second, Jobs: 4, Shell: []string{"sh", "-c"}}
}

func TestRunOutcomes(t *testing.T) {
	retired := page("old", "true", "")
	retired.Fields["status"] = "retired"
	pages := []*Page{
		page("ok", "true", ""),
		page("bad", "echo 'no such thing' >&2; exit 3", ""),
		page("doc", "none", ""),
		page("missing", "", ""),
		retired,
	}
	res := testRunner().Run(context.Background(), pages)
	want := []string{Verified, Failing, Doc, NoCheck, Retired}
	for i, w := range want {
		if res[i].Outcome != w {
			t.Errorf("%s: outcome %s, want %s (%s)", pages[i].ID, res[i].Outcome, w, res[i].Reason)
		}
	}
	if res[1].ExitCode != 3 || res[1].Reason != "exit 3: no such thing" {
		t.Errorf("bad: exit %d reason %q", res[1].ExitCode, res[1].Reason)
	}
	if got := Summary(res); got != "verified=1 failing=1 unknown=0 doc=1 retired=1 nocheck=1" {
		t.Errorf("summary %q", got)
	}
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	r := testRunner()
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	res := r.Run(context.Background(), []*Page{page("slow", "sleep 30 | cat", "")})
	if res[0].Outcome != Failing || !strings.HasPrefix(res[0].Reason, "timed out after") {
		t.Fatalf("got %s %q", res[0].Outcome, res[0].Reason)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("timeout did not kill the pipeline: took %s", took)
	}
}

func TestRunNeverKeepsStdout(t *testing.T) {
	res := testRunner().Run(context.Background(), []*Page{page("loud", "echo SECRET-VALUE; echo oops >&2; exit 1", "")})
	if strings.Contains(res[0].Reason+res[0].Stderr, "SECRET-VALUE") {
		t.Fatalf("stdout leaked into the result: %+v", res[0])
	}
}

func TestRedact(t *testing.T) {
	in := "auth failed for FAKE0TOKEN0ab12cd34ef56gh78ij90 at /home/user/some-long-directory-name"
	got := redact(in)
	if strings.Contains(got, "FAKE0TOKEN") {
		t.Errorf("token kept: %q", got)
	}
	if !strings.Contains(got, "some-long-directory-name") {
		t.Errorf("letters-only word redacted: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("a\nb   c", 10); got != "a b c" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 100)
	got := truncate(long, 21)
	if !strings.HasSuffix(got, "...") || len(got) > 21 || !strings.HasPrefix(got, "é") {
		t.Errorf("got %q (%d bytes)", got, len(got))
	}
}

func TestSSHArgsQuoting(t *testing.T) {
	args := sshArgs("mac", `cd ~/x && echo 'hi' | grep -q "hi"`)
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "mac", `bash -lc 'cd ~/x && echo '\''hi'\'' | grep -q "hi"'`}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q", args)
	}
}

// fakeSSH writes a stand-in for ssh: host "down" never answers (exit 255),
// any other host runs the remote string locally with sh.
func fakeSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
while [ "$1" = "-o" ]; do shift 2; done
host=$1; shift
[ "$host" = down ] && { echo "ssh: connect to host down: No route to host" >&2; exit 255; }
exec sh -c "$1"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunRemoteAndUnreachable(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("needs bash for the remote side")
	}
	r := testRunner()
	r.SSH = fakeSSH(t)
	pages := []*Page{
		page("remote-ok", "test 'a b' = 'a b'", "box"),
		page("remote-bad", "exit 4", "box"),
		page("remote-255", "exit 255", "box"),
		page("on-down", "true", "down"),
	}
	res := r.Run(context.Background(), pages)
	want := []string{Verified, Failing, Failing, Unknown}
	for i, w := range want {
		if res[i].Outcome != w {
			t.Errorf("%s: %s, want %s (%s)", pages[i].ID, res[i].Outcome, w, res[i].Reason)
		}
	}
	if res[3].Reason != "host down unreachable" {
		t.Errorf("reason %q", res[3].Reason)
	}
}

func TestWriteResults(t *testing.T) {
	dir := t.TempDir()
	okPath := writePage(t, dir, "ok.md", "---\nid: ok\nstatus: unverified\ncheck: 'true'\n---\nbody\n")
	badPath := writePage(t, dir, "bad.md", "---\nid: bad\nstatus: verified\nlast_verified: 2026-01-01\ncheck: 'exit 2'\n---\nbody\n")
	docPath := writePage(t, dir, "doc.md", "---\nid: doc\nstatus: unverified\ncheck: none\n---\n")
	retPath := writePage(t, dir, "ret.md", "---\nid: ret\nstatus: retired\ncheck: none\n---\n")
	pages, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := testRunner().Run(context.Background(), pages)
	changed, err := WriteResults(res, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Errorf("changed %v", changed)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if got := read(okPath); got != "---\nid: ok\nstatus: verified\nlast_verified: 2026-11-09\ncheck: 'true'\n---\nbody\n" {
		t.Errorf("ok page:\n%s", got)
	}
	if got := read(badPath); got != "---\nid: bad\nstatus: failing\nlast_verified: 2026-01-01\nlast_checked: 2026-11-09\nlast_error: \"exit 2\"\ncheck: 'exit 2'\n---\nbody\n" {
		t.Errorf("bad page:\n%s", got)
	}
	if got := read(docPath); !strings.Contains(got, "status: doc\n") {
		t.Errorf("doc page:\n%s", got)
	}
	if got := read(retPath); got != "---\nid: ret\nstatus: retired\ncheck: none\n---\n" {
		t.Errorf("retired page changed:\n%s", got)
	}
	// A second write with the same results changes nothing.
	if again, _ := WriteResults(res, "2026-11-09"); len(again) != 0 {
		t.Errorf("second write changed %v", again)
	}
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "ok.md", "---\nid: ok\nstatus: unverified\ncheck: 'true'\n---\n")
	writePage(t, dir, "bad.md", "---\nid: bad\nstatus: unverified\ncheck: 'echo hidden-output; exit 7'\n---\n")
	var out, errb bytes.Buffer

	if code := run([]string{"list", "--dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "ok") || !strings.Contains(out.String(), "LAST_VERIFIED") {
		t.Errorf("list output:\n%s", out.String())
	}

	out.Reset()
	code := run([]string{"check", "ok", "--dir", dir, "bad", "--write"}, &out, &errb)
	if code != 1 {
		t.Fatalf("check exit %d, want 1 (a failure): %s", code, errb.String())
	}
	s := out.String()
	if strings.Contains(s, "hidden-output") {
		t.Fatalf("check printed command output:\n%s", s)
	}
	if !strings.Contains(strings.Join(strings.Fields(s), " "), "failing bad exit 7") || !strings.Contains(s, "verified=1 failing=1") || !strings.Contains(s, "wrote 2 pages") {
		t.Errorf("check output:\n%s", s)
	}

	if code := run([]string{"check", "--dir", dir, "nope"}, &out, &errb); code != 2 {
		t.Errorf("unknown id: exit %d, want 2", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
}
