// Package prompt asks questions on a terminal. It reads line by line from any
// reader, so it can be driven by tests and piped input.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrAborted is returned when input ends (Ctrl-D) before an answer is given.
var ErrAborted = errors.New("aborted")

type Prompter struct {
	in  *bufio.Reader
	out io.Writer
	fd  int // terminal file descriptor for hidden input, or -1
}

// New returns a Prompter. If in is a terminal, secrets are read without echo.
func New(in io.Reader, out io.Writer) *Prompter {
	fd := -1
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd = int(f.Fd())
	}
	return &Prompter{in: bufio.NewReader(in), out: out, fd: fd}
}

// IsTerminal reports whether f is an interactive terminal.
func IsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func (p *Prompter) Printf(format string, a ...any) { fmt.Fprintf(p.out, format, a...) }

func (p *Prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(p.out)
			return "", ErrAborted
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Validator returns an error describing why an answer is unacceptable.
type Validator func(string) error

// Input asks for a line of text. An empty answer returns def. A non-nil
// validate is applied to the final value and the question repeats on error.
func (p *Prompter) Input(label, def string, validate Validator) (string, error) {
	for {
		if def != "" {
			p.Printf("%s [%s]: ", label, def)
		} else {
			p.Printf("%s: ", label)
		}
		v, err := p.readLine()
		if err != nil {
			return "", err
		}
		if v == "" {
			v = def
		}
		if validate != nil {
			if err := validate(v); err != nil {
				p.Printf("  ✗ %v\n", err)
				continue
			}
		}
		return v, nil
	}
}

// Required is a Validator that rejects empty answers.
func Required(v string) error {
	if v == "" {
		return errors.New("a value is required")
	}
	return nil
}

// Secret asks for a value without echoing it on a terminal. If current is
// set, an empty answer keeps it.
func (p *Prompter) Secret(label, current string, validate Validator) (string, error) {
	for {
		if current != "" {
			p.Printf("%s [keep current]: ", label)
		} else {
			p.Printf("%s: ", label)
		}
		var v string
		if p.fd >= 0 {
			b, err := term.ReadPassword(p.fd)
			fmt.Fprintln(p.out)
			if err != nil {
				return "", err
			}
			v = strings.TrimSpace(string(b))
		} else {
			var err error
			if v, err = p.readLine(); err != nil {
				return "", err
			}
		}
		if v == "" {
			v = current
		}
		if validate != nil {
			if err := validate(v); err != nil {
				p.Printf("  ✗ %v\n", err)
				continue
			}
		}
		return v, nil
	}
}

// Confirm asks a yes/no question.
func (p *Prompter) Confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		p.Printf("%s [%s]: ", label, hint)
		v, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(v) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Printf("  ✗ answer y or n\n")
	}
}

// Select shows numbered options and returns the index chosen. def is the
// index used for an empty answer, or -1 to require a choice.
func (p *Prompter) Select(label string, options []string, def int) (int, error) {
	p.Printf("%s\n", label)
	for i, o := range options {
		p.Printf("  %d) %s\n", i+1, o)
	}
	for {
		if def >= 0 {
			p.Printf("Choose 1-%d [%d]: ", len(options), def+1)
		} else {
			p.Printf("Choose 1-%d: ", len(options))
		}
		v, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if v == "" && def >= 0 {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		p.Printf("  ✗ enter a number from 1 to %d\n", len(options))
	}
}

// List collects values one per line until an empty answer.
func (p *Prompter) List(label string, validate Validator) ([]string, error) {
	var out []string
	for {
		v, err := p.Input(label+" (blank to finish)", "", func(s string) error {
			if s == "" || validate == nil {
				return nil
			}
			return validate(s)
		})
		if err != nil {
			return out, err
		}
		if v == "" {
			return out, nil
		}
		out = append(out, v)
	}
}
