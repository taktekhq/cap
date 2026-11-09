// Command cap keeps capability notes honest: each Markdown page carries a
// check command in its front matter, and cap runs the checks and records
// the result in the page.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const version = "0.1.0"

const usage = `cap: run the check in each capability page and record the result.

Usage:
  cap list  [--dir DIR] [--status S]
  cap check [--dir DIR] [--write] [--verbose] [--timeout 30s] [--jobs 4] [id...]
  cap version

Each page is a Markdown file with YAML front matter. cap reads these fields:
  id:        the page id (default: the file name without .md)
  check:     a shell command; exit 0 means the capability works; "none" for docs
  check_on:  local (default) or an ssh host such as mac
  status:    verified, failing, unknown, doc or retired (retired pages never run)

cap check never prints a check's output. --verbose adds the tail of stderr.
--write records the result in the front matter and changes nothing else:
a pass sets status: verified and last_verified (today); a failure sets
status: failing, last_checked and a one-line last_error; an unreachable
host sets status: unknown the same way; check: none sets status: doc.

Flags:
  --dir DIR      the folder of pages (default $CAP_DIR, else .)
  --status S     list only pages with this status (list)
  --write        record results in the pages (check)
  --verbose      print each failing check's stderr, truncated (check)
  --timeout D    per check (default 30s)
  --jobs N       checks in parallel (default 4)

Exit status: 0 ok, 1 a check failed, 2 usage or file error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "list":
		return cmdList(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "cap", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "cap: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func defaultDir() string {
	if d := os.Getenv("CAP_DIR"); d != "" {
		return d
	}
	return "."
}

// parseInterleaved lets flags and ids come in any order: cap check a --write b.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir(), "")
	status := fs.String("status", "", "")
	if _, err := parseInterleaved(fs, args); err != nil {
		return 2
	}
	pages, err := LoadDir(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "cap:", err)
		return 2
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tON\tLAST_VERIFIED\tLAST_CHECKED\tCHECK")
	for _, p := range pages {
		if *status != "" && !strings.EqualFold(p.Get("status"), *status) {
			continue
		}
		on := p.Host()
		if on == "" {
			on = "local"
		}
		chk := "yes"
		switch {
		case p.Check() == "":
			chk, on = "missing", "-"
		case p.IsDoc():
			chk, on = "none", "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, dash(p.Get("status")), on,
			dash(p.Get("last_verified")), dash(p.Get("last_checked")), chk)
	}
	tw.Flush()
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir(), "")
	write := fs.Bool("write", false, "")
	verbose := fs.Bool("verbose", false, "")
	timeout := fs.Duration("timeout", 30*time.Second, "")
	jobs := fs.Int("jobs", 4, "")
	ids, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	pages, err := LoadDir(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "cap:", err)
		return 2
	}
	if len(ids) > 0 {
		byID := map[string]*Page{}
		for _, p := range pages {
			byID[p.ID] = p
		}
		var pick []*Page
		for _, id := range ids {
			p, ok := byID[strings.TrimSuffix(id, ".md")]
			if !ok {
				fmt.Fprintf(stderr, "cap: no page with id %q in %s\n", id, *dir)
				return 2
			}
			pick = append(pick, p)
		}
		pages = pick
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &Runner{Timeout: *timeout, Jobs: *jobs, SSH: os.Getenv("CAP_SSH")}
	results := r.Run(ctx, pages)

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, res := range results {
		if res.Outcome == NoCheck && len(ids) == 0 {
			continue // listed by the summary; ask for an id to see it
		}
		why := ""
		switch res.Outcome {
		case Failing:
			why = shortReason(res)
		case Unknown:
			why = res.Reason
		}
		if why == "" {
			fmt.Fprintf(tw, "%s\t%s\n", res.Outcome, res.Page.ID)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", res.Outcome, res.Page.ID, why)
		}
		if *verbose && res.Outcome == Failing && res.Stderr != "" {
			tw.Flush()
			for _, l := range strings.Split(tail(res.Stderr, 400), "\n") {
				fmt.Fprintln(stdout, "    | "+l)
			}
		}
	}
	tw.Flush()
	fmt.Fprintln(stdout, Summary(results))

	if *write {
		today := time.Now().Format("2006-01-02")
		changed, err := WriteResults(results, today)
		if err != nil {
			fmt.Fprintln(stderr, "cap:", err)
			return 2
		}
		fmt.Fprintf(stdout, "wrote %d pages\n", len(changed))
	}
	if Counts(results)[Failing] > 0 {
		return 1
	}
	return 0
}

// shortReason is what cap prints by default: the exit status only, never output.
func shortReason(res Result) string {
	switch {
	case res.ExitCode > 0:
		return fmt.Sprintf("exit %d", res.ExitCode)
	case strings.HasPrefix(res.Reason, "timed out"), strings.HasPrefix(res.Reason, "could not start"):
		return res.Reason
	}
	return "killed"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return "..." + s[cut:]
}
