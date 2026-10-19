//go:build unix

// Command compass-forge-credential routes Git and jj-vine credentials by repository owner.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // gh is the default-installation token source, as in the image's prior tokenCommand.
	"strings"
	"time"

	"github.com/RigelBuild/compass/go/internal/forgecred"
)

// ghBinary is set to the image's pinned gh path at build time when available.
var ghBinary string

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, in io.Reader, out, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "a command is required: git-credential or token")
		return 2
	}
	home := os.Getenv("HOME")
	switch args[0] {
	case "git-credential":
		if err := gitCredential(home, args[1:], in, out, stderr); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	case "token":
		if err := token(home, args[1:], out, stderr); err != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				return exitErr.ExitCode()
			}
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func gitCredential(home string, args []string, in io.Reader, out, stderr io.Writer) error {
	if len(args) != 1 {
		return errors.New("git-credential requires get, store, or erase")
	}
	switch args[0] {
	case "store", "erase":
		return nil
	case "get":
	default:
		return fmt.Errorf("unknown git-credential action %q", args[0])
	}
	fields, err := readCredentialInput(in)
	if err != nil {
		return err
	}
	host := fields["host"]
	owner, ok := forgecred.OwnerFromCredentialPath(fields["path"])
	if !ok || host == "" {
		return nil
	}
	defaultOwner, hasDefault, err := forgecred.DefaultOwner(home, host)
	if err != nil {
		return err
	}
	if !hasDefault || owner == defaultOwner {
		return nil
	}
	token, found, err := forgecred.LookupToken(home, host, owner)
	if err != nil {
		return err
	}
	if found {
		_, err := fmt.Fprintf(out, "username=x-access-token\npassword=%s\n\n", token)
		return err
	}
	if _, err := io.WriteString(out, "quit=1\n\n"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "no credential for owner %s\n", owner)
	return err
}

func token(home string, args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "Forge hostname")
	repo := fs.String("repo", "", "Target repository in owner/name form")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *host == "" {
		return errors.New("--host is required")
	}
	owner := ""
	if *repo != "" {
		var ok bool
		owner, ok = forgecred.OwnerFromCredentialPath(*repo)
		if !ok {
			return errors.New("--repo must be in owner/name form")
		}
	} else {
		defaultOwner, ok, err := forgecred.DefaultOwner(home, *host)
		if err != nil {
			return err
		}
		if ok {
			owner = defaultOwner
		}
	}
	if owner == "" {
		return runGHToken(*host, out, stderr)
	}

	defaultOwner, hasDefault, err := forgecred.DefaultOwner(home, *host)
	if err != nil {
		return err
	}
	if !hasDefault {
		return runGHToken(*host, out, stderr)
	}
	credential, found, err := forgecred.LookupToken(home, *host, owner)
	if err != nil {
		return err
	}
	if found {
		_, err := io.WriteString(out, credential)
		return err
	}
	if owner == defaultOwner {
		return runGHToken(*host, out, stderr)
	}
	return fmt.Errorf("no credential for owner %s", owner)
}

func readCredentialInput(in io.Reader) (map[string]string, error) {
	fields := make(map[string]string)
	var line strings.Builder
	for {
		line.Reset()
		for {
			var one [1]byte
			n, err := in.Read(one[:])
			if n > 0 {
				if one[0] == '\n' {
					break
				}
				line.WriteByte(one[0])
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return fields, nil
				}
				return nil, fmt.Errorf("reading Git credential input: %w", err)
			}
		}
		current := strings.TrimSuffix(line.String(), "\r")
		if current == "" {
			return fields, nil
		}
		key, value, ok := strings.Cut(current, "=")
		if ok {
			fields[key] = value
		}
	}
}

func runGHToken(host string, out, stderr io.Writer) error {
	binary := ghBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("gh")
		if err != nil {
			return errors.New("gh is not available on PATH")
		}
	}
	// git waits on its helper without a limit, so a wedged gh must not hang every fetch.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "auth", "token", "--hostname", host) //nolint:gosec // binary is the build-pinned gh or PATH's gh; args are fixed.
	cmd.Stdout = out
	cmd.Stderr = stderr
	return cmd.Run()
}
