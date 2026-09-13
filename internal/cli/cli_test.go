package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverIncludesAndConditionalCandidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	base := filepath.Join(dir, ".ssh")
	writeTestFile(t, filepath.Join(base, "config"), "Host prod PROD *.example !excluded\nHost=stage stage-2 # comment\nInclude \"conf.d/*.conf\"\nMatch exec \"touch /must-not-run\"\n Include conditional\n")
	writeTestFile(t, filepath.Join(base, "conf.d/a.conf"), "Host db\nInclude nested\n")
	writeTestFile(t, filepath.Join(base, "nested"), "Host nested-host\n")
	writeTestFile(t, filepath.Join(base, "conditional"), "Host conditional-host\n")
	hosts, warnings, err := discover([]configFile{{path: filepath.Join(base, "config"), includeBase: base}})
	want := []string{"conditional-host", "db", "nested-host", "prod", "stage", "stage-2"}
	if err != nil || len(warnings) != 0 || !reflect.DeepEqual(hosts, want) {
		t.Fatalf("got %v, %v, %v; want %v", hosts, warnings, err, want)
	}
}

func TestDiscoveryCycleAndIncompleteInventory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	writeTestFile(t, p, "Include config\n")
	if _, _, err := discover([]configFile{{path: p, includeBase: dir}}); err == nil {
		t.Fatal("expected cycle rejection")
	}
	writeTestFile(t, p, "Host good\nInclude %h.config\n")
	var out, diag bytes.Buffer
	code := Run(context.Background(), []string{"hosts", "-F", p}, nil, &out, &diag, "test")
	if code != 1 || out.String() != "good\n" || !strings.Contains(diag.String(), "token") {
		t.Fatalf("partial inventory: code=%d out=%q diag=%q", code, &out, &diag)
	}
}

func TestHostsDoesNotEvaluateMatchExec(t *testing.T) {
	dir := t.TempDir()
	p, marker := filepath.Join(dir, "config"), filepath.Join(dir, "marker")
	writeTestFile(t, p, "Match exec \"touch "+marker+"\"\nHost listed\n")
	var out, diag bytes.Buffer
	if code := Run(context.Background(), []string{"hosts", "LIST", "-F", p}, nil, &out, &diag, "test"); code != 0 || out.String() != "listed\n" {
		t.Fatalf("hosts: %d %s %s", code, &out, &diag)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Match exec was evaluated")
	}
}

func TestConfigWords(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`"path with spaces" another\ path # comment`, []string{"path with spaces", "another path"}},
		{`"hash#name" 'quoted'`, []string{"hash#name", "quoted"}},
	} {
		got, err := configWords(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %v %v", tc.input, got, err)
		}
	}
	if _, err := configWords(`"unfinished`); err == nil {
		t.Fatal("expected quote error")
	}
}

func TestLimiterDrainsAndReportsDroppedBytes(t *testing.T) {
	var out bytes.Buffer
	w := &limitedWriter{out: &out, limit: 5}
	for _, p := range []string{"abc", "defgh", "more"} {
		if n, err := w.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("short drain: %d %v", n, err)
		}
	}
	if out.String() != "abcde" || w.dropped != 7 {
		t.Fatalf("%q dropped=%d", &out, w.dropped)
	}
	w = &limitedWriter{out: io.Discard}
	if _, err := io.Copy(w, strings.NewReader(strings.Repeat("x", 1<<20))); err != nil || w.dropped != 0 {
		t.Fatal("unlimited stream truncated")
	}
}

func TestArgumentParsingPreservesRemoteCommand(t *testing.T) {
	var diag bytes.Buffer
	f := flags("exec", "test", &diag)
	c := f.String("command", "", "")
	debug := f.Bool("debug", false, "")
	want := "-literal 'quotes' $HOME $(touch no)\nsecond line"
	if err := parse(f, []string{"host", "--command", want, "--debug"}); err != nil {
		t.Fatal(err)
	}
	if *c != want || !*debug || f.Arg(0) != "host" || f.NArg() != 1 {
		t.Fatalf("command=%q args=%v", *c, f.Args())
	}
}

func TestInvalidExecNeverStartsSSH(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "host"},
		{"exec", "--command", "true", "--", "-oProxyCommand=evil"},
		{"exec", "a b", "--command", "true"},
		{"exec", "*", "--command", "true"},
		{"exec", "host", "--command", "true", "--timeout", "-1s"},
		{"exec", "host", "--command", "true", "--max-output", "-1"},
		{"exec", "host", "--command", "true", "--json"},
	} {
		var out, diag bytes.Buffer
		if code := Run(context.Background(), args, nil, &out, &diag, "test"); code != 2 {
			t.Errorf("%v: code %d diag %q", args, code, &diag)
		}
	}
}

func TestPrivateRuntimeAndCancelableLock(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sshm-unit-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHM_RUNTIME_DIR", dir)
	if _, err := privateDir(true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.lock")
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock should honor cancellation: %v", err)
	}
	unlock()
	unlock, err = acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := privateDir(true); err == nil {
		t.Fatal("accepted nonprivate dir")
	}
	os.Chmod(dir, 0700)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(context.Background(), link); err == nil {
		t.Fatal("followed lock symlink")
	}
}

func TestControlIdentityChangesWithConfigAgentAndTTL(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/agent-a")
	p := controlPath("/tmp", []byte("user one\n"), time.Minute)
	if p != controlPath("/tmp", []byte("user one\n"), time.Minute) {
		t.Fatal("unstable key")
	}
	if p == controlPath("/tmp", []byte("user two\n"), time.Minute) {
		t.Fatal("mixed users")
	}
	if p == controlPath("/tmp", []byte("user one\n"), time.Second) {
		t.Fatal("mixed TTLs")
	}
	t.Setenv("SSH_AUTH_SOCK", "/agent-b")
	if p == controlPath("/tmp", []byte("user one\n"), time.Minute) {
		t.Fatal("mixed agents")
	}
}

func TestDeadlineBeforeStartIsNotReportedAsRemoteExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var diag bytes.Buffer
	if code := localFailure(ctx, &diag, ctx.Err(), false); code != 130 || !strings.Contains(diag.String(), "尚未启动") {
		t.Fatalf("%d %s", code, &diag)
	}
}

func TestFIFOsCannotBlockInputOrDiscovery(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discover([]configFile{{path: fifo, includeBase: dir}}); err == nil {
		t.Fatal("accepted FIFO config")
	}
	// Only LookPath is needed; the fake executable must never be started.
	writeTestFile(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nexit 99\n")
	if err := os.Chmod(filepath.Join(dir, "ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out, diag bytes.Buffer
	if code := Run(context.Background(), []string{"exec", "host", "--command", "true", "--stdin", fifo}, nil, &out, &diag, "test"); code != 2 {
		t.Fatalf("FIFO input: %d %s", code, &diag)
	}
}
