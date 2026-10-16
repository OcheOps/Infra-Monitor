package prompt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func newP(input string) (*Prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return New(strings.NewReader(input), &out), &out
}

func TestInputDefaultAndValidation(t *testing.T) {
	p, out := newP("\nbad\ngood\n")
	v, err := p.Input("Name", "dflt", nil)
	if err != nil || v != "dflt" {
		t.Fatalf("got %q, %v; want default", v, err)
	}
	v, err = p.Input("Name", "", func(s string) error {
		if s != "good" {
			return errors.New("nope")
		}
		return nil
	})
	if err != nil || v != "good" {
		t.Fatalf("got %q, %v", v, err)
	}
	if !strings.Contains(out.String(), "✗ nope") {
		t.Errorf("validation error not shown:\n%s", out.String())
	}
}

func TestConfirm(t *testing.T) {
	p, _ := newP("\nmaybe\nY\nno\n")
	for _, want := range []bool{true, true, false} {
		got, err := p.Confirm("ok?", true)
		if err != nil || got != want {
			t.Fatalf("got %v, %v; want %v", got, err, want)
		}
	}
}

func TestSelect(t *testing.T) {
	p, _ := newP("9\n2\n\n")
	if i, err := p.Select("Pick", []string{"a", "b", "c"}, -1); err != nil || i != 1 {
		t.Fatalf("got %d, %v", i, err)
	}
	if i, err := p.Select("Pick", []string{"a", "b"}, 0); err != nil || i != 0 {
		t.Fatalf("default: got %d, %v", i, err)
	}
}

func TestList(t *testing.T) {
	p, _ := newP("one\ntwo\n\n")
	got, err := p.List("Item", nil)
	if err != nil || strings.Join(got, ",") != "one,two" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSecretKeepsCurrent(t *testing.T) {
	p, _ := newP("\n")
	v, err := p.Secret("Password", "old", nil)
	if err != nil || v != "old" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestAbortOnEOF(t *testing.T) {
	p, _ := newP("")
	if _, err := p.Input("x", "", nil); !errors.Is(err, ErrAborted) {
		t.Fatalf("got %v, want ErrAborted", err)
	}
}

func TestLastLineWithoutNewline(t *testing.T) {
	p, _ := newP("value")
	if v, err := p.Input("x", "", nil); err != nil || v != "value" {
		t.Fatalf("got %q, %v", v, err)
	}
}
