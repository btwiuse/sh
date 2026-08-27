// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btwiuse/sh/v3/interp"
)

// TestFDRedirectWriteAndClose covers the basic fd>2 redirection forms:
// `exec 3<>file` opens a read-write fd that persists across statements
// (keepRedirs), `>&3` writes to it, and `3<&-` closes it.
func TestFDRedirectWriteAndClose(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	path := filepath.Join(tdir, "out.txt")
	runFDShell(t, tdir, `
exec 3<>`+path+`
echo "hello fd3" >&3
echo "second line" >&3
exec 3<&-
echo "closed"
`)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "hello fd3\nsecond line\n"; got != want {
		t.Fatalf("fd3 write mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

// TestFDRedirectReadDup covers `<&3` duplicating a stored fd onto stdin,
// with the shared file offset persisting across statements and function
// calls, and `3<&-` closing the fd.
func TestFDRedirectReadDup(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	input := filepath.Join(tdir, "in.txt")
	if err := os.WriteFile(input, []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var cb bytes.Buffer
	runFDShellTo(t, tdir, &cb, `
exec 3<`+input+`
read -r a <&3
read -r b <&3
printf "%s|%s\n" "$a" "$b"
exec 3<&-
echo "done"
`)
	if got, want := cb.String(), "line-one|line-two\ndone\n"; got != want {
		t.Fatalf("fd3 read mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

// TestFDRedirectRdrInOutOnStdin covers `0<>file` (the `<>` operator on the
// default fd), which must open the file read-write and keep stdin usable.
func TestFDRedirectRdrInOutOnStdin(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	input := filepath.Join(tdir, "in.txt")
	if err := os.WriteFile(input, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var cb bytes.Buffer
	runFDShellTo(t, tdir, &cb, `
exec 0<>`+input+`
read -r x
printf "got=%s\n" "$x"
`)
	if got, want := cb.String(), "got=first\n"; got != want {
		t.Fatalf("stdin <> mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

// TestFDRedirectBadFd asserts that referencing an fd that was never opened
// fails the statement (visible via $?) instead of panicking or silently
// writing nowhere.
func TestFDRedirectBadFd(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	var cb bytes.Buffer
	runFDShellTo(t, tdir, &cb, `
echo hi >&4
echo "rc=$?"
`)
	if got, want := cb.String(), "rc=1\n"; got != want {
		t.Fatalf("bad fd handling mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

// funcFileSim emulates the write-once/read-once contract of the wanix jsfs
// funcfile that GearShell's gctl protocol relies on: a written line invokes
// the underlying JS function and queues exactly one response, which a read
// returns before EOF.
type funcFileSim struct {
	buf    []byte
	result []byte
	eof    bool
}

func (s *funcFileSim) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	s.result = []byte(`{"ok":true}`)
	s.eof = false
	return len(p), nil
}

func (s *funcFileSim) Read(p []byte) (int, error) {
	if s.eof {
		return 0, io.EOF
	}
	n := copy(p, s.result)
	s.eof = true
	return n, nil
}

func (s *funcFileSim) Close() error { return nil }

// TestFDRedirectFuncFileProtocol runs the exact gctl request/response
// protocol over an fd>2 read-write redirection against a jsfs-like funcfile
// (simulated through the OpenHandler), proving `exec 3<>` + `>&3` + `<&3`
// round-trips a call and its result on a single fd.
func TestFDRedirectFuncFileProtocol(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	var cb bytes.Buffer
	runner, err := interp.New(
		interp.Dir(tdir),
		interp.StdIO(nil, &cb, &cb),
		interp.OpenHandler(func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
			if strings.HasPrefix(path, "fake://") {
				return &funcFileSim{}, nil
			}
			return os.OpenFile(path, flag, perm)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := parse(t, nil, `
exec 3<>fake://GearShell/panels/list.json
echo '[]' >&3
read -r out <&3
printf '%s\n' "$out"
exec 3<&-
`)
	if err := runner.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	if got, want := cb.String(), `{"ok":true}`+"\n"; got != want {
		t.Fatalf("funcfile protocol mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

// runFDShell runs a script with the default exec handler and asserts it
// completes without a runner error.
func runFDShell(t *testing.T, dir, src string) {
	t.Helper()
	var cb bytes.Buffer
	runFDShellTo(t, dir, &cb, src)
}

func runFDShellTo(t *testing.T, dir string, cb *bytes.Buffer, src string) {
	t.Helper()
	runner, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(nil, cb, cb),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := parse(t, nil, src)
	if err := runner.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
}
