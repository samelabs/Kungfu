package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"kungfu.md/internal/admin"
	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
)

// adminctl — Platform Admin CLI.
//
// bootstrap: creates the FIRST platform administrator (superadmin) or
// fails closed when one already exists. The password is read from
// stdin, never from argv and never from the environment.

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bootstrap":
		if err := runBootstrap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "adminctl: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  adminctl bootstrap --username <username> --display-name <name> [--password-stdin]

The bootstrap password is always read from stdin (optionally
--password-stdin for pipelines); it is never accepted as a command-line
argument or environment variable.
`)
}

func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	username := fs.String("username", "", "admin username (3-64 chars of a-z0-9._-)")
	displayName := fs.String("display-name", "", "display name")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin (default behavior; flag kept for pipeline clarity)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *displayName == "" {
		return fmt.Errorf("--username and --display-name are required")
	}
	_ = passwordStdin // stdin is ALWAYS the password source; the flag is documentation only

	password, err := readPassword(os.Stdin)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("password: read from stdin is empty")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	pool, err := pg.NewPool(cfg.DatabaseURL())
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	result, err := admin.Bootstrap(context.Background(), pool, *username, *displayName, password)
	if err != nil {
		return err
	}
	fmt.Printf("bootstrap: admin %q (id=%d) created with superadmin role\n", result.Username, result.AdminID)
	return nil
}

// readPassword reads the password from stdin: a single line when the
// input is a terminal-like one-liner, or the whole piped stream
// (trimmed of the final newline) otherwise.
func readPassword(r io.Reader) (string, error) {
	stat, ok := any(r).(interface{ Stat() (os.FileInfo, error) })
	if ok {
		if fi, err := stat.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			// interactive: read one line
			line, err := bufio.NewReader(r).ReadString('\n')
			return strings.TrimSpace(line), err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
