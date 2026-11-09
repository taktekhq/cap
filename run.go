package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Outcome of one page.
const (
	Verified = "verified" // the check exited 0
	Failing  = "failing"  // the check ran and exited non-zero, or timed out
	Unknown  = "unknown"  // the check could not run: its ssh host did not answer
	Doc      = "doc"      // check: none
	Retired  = "retired"  // status: retired, never run
	NoCheck  = "nocheck"  // no check field yet
)

var outcomeOrder = []string{Verified, Failing, Unknown, Doc, Retired, NoCheck}

// Result of one page.
type Result struct {
	Page     *Page
	Outcome  string
	Reason   string // one line for last_error: "exit 1: <stderr line>", "timed out after 30s", "host mac unreachable"
	Stderr   string // tail of stderr, redacted; printed only with --verbose
	ExitCode int
	Took     time.Duration
}

// Runner runs checks.
type Runner struct {
	Timeout time.Duration // per check
	Jobs    int           // checks in parallel
	Shell   []string      // local shell, default bash -lc
	SSH     string        // ssh binary, default "ssh"
	Dir     string        // working directory for local checks, default $HOME
}

func (r *Runner) shell() []string {
	if len(r.Shell) > 0 {
		return r.Shell
	}
	return []string{"bash", "-lc"}
}

func (r *Runner) ssh() string {
	if r.SSH != "" {
		return r.SSH
	}
	return "ssh"
}

// sshArgs is the argument list for running cmd on host. The remote side gets
// one string, "bash -lc '<cmd>'", so the remote login shell (zsh, bash) only
// has to undo one layer of single quotes.
func sshArgs(host, cmd string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, "bash -lc " + shellQuote(cmd)}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Run classifies every page and runs the checks that need running.
func (r *Runner) Run(ctx context.Context, pages []*Page) []Result {
	results := make([]Result, len(pages))
	var todo []int
	hosts := map[string]bool{}
	for i, p := range pages {
		results[i].Page = p
		switch {
		case p.IsRetired():
			results[i].Outcome = Retired
		case p.Check() == "":
			results[i].Outcome = NoCheck
		case p.IsDoc():
			results[i].Outcome = Doc
		default:
			todo = append(todo, i)
			if h := p.Host(); h != "" {
				hosts[h] = true
			}
		}
	}

	// One reachability probe per ssh host, before any check runs there.
	up := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			ok := r.reachable(ctx, h)
			mu.Lock()
			up[h] = ok
			mu.Unlock()
		}(h)
	}
	wg.Wait()

	jobs := r.Jobs
	if jobs < 1 {
		jobs = 1
	}
	sem := make(chan struct{}, jobs)
	for _, i := range todo {
		p := pages[i]
		if h := p.Host(); h != "" && !up[h] {
			results[i].Outcome = Unknown
			results[i].Reason = "host " + h + " unreachable"
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			res := r.runOne(ctx, pages[i])
			// ssh exits 255 when the connection fails. If the host is gone now,
			// that is unknown, not a failing capability.
			if h := pages[i].Host(); h != "" && res.Outcome == Failing && res.ExitCode == 255 && !r.reachable(ctx, h) {
				res.Outcome = Unknown
				res.Reason = "host " + h + " unreachable"
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	return results
}

func (r *Runner) reachable(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.ssh(), "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, "true")
	setGroupKill(cmd)
	return cmd.Run() == nil
}

func (r *Runner) runOne(ctx context.Context, p *Page) Result {
	res := Result{Page: p}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if h := p.Host(); h != "" {
		cmd = exec.CommandContext(ctx, r.ssh(), sshArgs(h, p.Check())...)
	} else {
		sh := r.shell()
		cmd = exec.CommandContext(ctx, sh[0], append(sh[1:], p.Check())...)
		cmd.Dir = r.Dir
		if cmd.Dir == "" {
			cmd.Dir, _ = os.UserHomeDir()
		}
	}
	// stdin and stdout go to /dev/null: stdout is never read, so it can never be shown.
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	setGroupKill(cmd)

	start := time.Now()
	err := cmd.Run()
	res.Took = time.Since(start)
	res.Stderr = redact(strings.TrimSpace(stderr.String()))
	switch {
	case err == nil:
		res.Outcome = Verified
	case ctx.Err() == context.DeadlineExceeded:
		res.Outcome = Failing
		res.ExitCode = -1
		res.Reason = "timed out after " + timeout.String()
	default:
		res.Outcome = Failing
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			res.Reason = fmt.Sprintf("exit %d", res.ExitCode)
		} else {
			res.ExitCode = -1
			res.Reason = "could not start: " + err.Error()
		}
		if line := lastLine(res.Stderr); line != "" {
			res.Reason += ": " + line
		}
	}
	res.Reason = truncate(res.Reason, 160)
	return res
}

// setGroupKill runs the command in its own process group and kills the whole
// group on timeout, so a pipeline or an ssh session does not outlive it.
func setGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// tokenish matches long runs that look like keys or tokens: 24+ characters of
// letters, digits, - _ + = with at least one letter and one digit.
var tokenish = regexp.MustCompile(`[A-Za-z0-9_\-+=]{24,}`)

// redact replaces anything token-shaped. A check should never write a secret
// to stderr; this is a second line of defence, not the first.
func redact(s string) string {
	return tokenish.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.ContainsAny(m, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			return "[redacted]"
		}
		return m
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not split a UTF-8 rune
		cut--
	}
	return s[:cut] + "..."
}

// Counts tallies outcomes.
func Counts(results []Result) map[string]int {
	c := map[string]int{}
	for _, r := range results {
		c[r.Outcome]++
	}
	return c
}

// Summary is one machine-readable line: "verified=3 failing=1 unknown=0 doc=2 retired=0 nocheck=0".
func Summary(results []Result) string {
	c := Counts(results)
	parts := make([]string, 0, len(outcomeOrder))
	for _, o := range outcomeOrder {
		parts = append(parts, fmt.Sprintf("%s=%d", o, c[o]))
	}
	return strings.Join(parts, " ")
}

// EditsFor is what --write changes in a page for its result.
func EditsFor(res Result, today string) []Edit {
	switch res.Outcome {
	case Verified:
		return []Edit{
			{Key: "status", Value: Verified},
			{Key: "last_verified", Value: today},
			{Key: "last_checked", Value: today},
			{Key: "last_error", Delete: true},
		}
	case Failing, Unknown:
		return []Edit{
			{Key: "status", Value: res.Outcome},
			{Key: "last_checked", Value: today},
			{Key: "last_error", Value: quoteDouble(res.Reason)},
		}
	case Doc:
		return []Edit{{Key: "status", Value: Doc}, {Key: "last_error", Delete: true}}
	}
	return nil
}

// WriteResults applies EditsFor to each page file. It returns the paths it changed.
func WriteResults(results []Result, today string) ([]string, error) {
	var changed []string
	for _, res := range results {
		edits := EditsFor(res, today)
		if len(edits) == 0 {
			continue
		}
		data, err := os.ReadFile(res.Page.Path)
		if err != nil {
			return changed, err
		}
		out, did, err := EditFrontMatter(data, edits)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", res.Page.Path, err)
		}
		if !did {
			continue
		}
		if err := writeFileAtomic(res.Page.Path, out); err != nil {
			return changed, err
		}
		changed = append(changed, res.Page.Path)
	}
	sort.Strings(changed)
	return changed, nil
}

func writeFileAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".cap-tmp"
	if err := os.WriteFile(tmp, data, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
