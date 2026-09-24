package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

type fixture struct {
	listener    net.Listener
	config      string
	active      atomic.Int64
	workers     sync.WaitGroup
	connections sync.Map
	password    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(key)
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{listener: l, password: "synthetic-unit-password"}
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	host, port, _ := net.SplitHostPort(l.Addr().String())
	os.WriteFile(known, []byte("["+host+"]:"+port+" "+string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), 0600)
	f.config = filepath.Join(dir, "servers.toml")
	os.WriteFile(f.config, []byte(fmt.Sprintf("known_hosts = %q\n[ssh_servers.test]\nhost = %q\nport = %s\nuser = 'tester'\npassword = %q\n", known, host, port, f.password)), 0600)
	sc := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if string(p) != f.password {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	sc.AddHostKey(signer)
	f.workers.Add(1)
	go func() {
		defer f.workers.Done()
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			f.connections.Store(raw, true)
			f.active.Add(1)
			f.workers.Add(1)
			go func() {
				defer f.workers.Done()
				defer f.active.Add(-1)
				defer f.connections.Delete(raw)
				defer raw.Close()
				c, chans, reqs, err := ssh.NewServerConn(raw, sc)
				if err != nil {
					return
				}
				defer c.Close()
				go ssh.DiscardRequests(reqs)
				var sessions sync.WaitGroup
				for ch := range chans {
					if ch.ChannelType() != "session" {
						ch.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					channel, requests, err := ch.Accept()
					if err != nil {
						continue
					}
					sessions.Add(1)
					go func() {
						defer sessions.Done()
						defer channel.Close()
						for r := range requests {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var request struct{ Command string }
							ssh.Unmarshal(r.Payload, &request)
							if request.Command == "no-ack" {
								io.Copy(io.Discard, channel)
								return
							}
							r.Reply(true, nil)
							code := uint32(0)
							switch {
							case request.Command == "cat":
								io.Copy(channel, channel)
							case request.Command == "hang":
								<-time.After(150 * time.Millisecond)
							case request.Command == "drop":
								return
							case strings.HasPrefix(request.Command, "exit:"):
								v, _ := strconv.Atoi(strings.TrimPrefix(request.Command, "exit:"))
								code = uint32(v)
							case strings.HasPrefix(request.Command, "out:"):
								n, _ := strconv.Atoi(strings.TrimPrefix(request.Command, "out:"))
								block := make([]byte, 32768)
								for n > 0 {
									k := min(n, len(block))
									if _, e := channel.Write(block[:k]); e != nil {
										return
									}
									n -= k
								}
								channel.Stderr().Write([]byte("diag"))
							default:
								channel.Write([]byte(request.Command))
							}
							data := make([]byte, 4)
							binary.BigEndian.PutUint32(data, code)
							channel.SendRequest("exit-status", false, data)
							return
						}
					}()
				}
				sessions.Wait()
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		f.connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		f.workers.Wait()
	})
	return f
}
func runFixture(f *fixture, command string, stdin *os.File, opts ...string) (int, []byte, []byte) {
	var out, err bytes.Buffer
	args := []string{"exec", "test", "-F", f.config, "--command", command, "--timeout", "3s"}
	args = append(args, opts...)
	code := Run(context.Background(), args, stdin, &out, &err, "test")
	return code, out.Bytes(), err.Bytes()
}
func TestRawOutputAndExitStatus(t *testing.T) {
	f := newFixture(t)
	for _, v := range []int{0, 1, 17, 124, 125, 130, 255} {
		code, out, err := runFixture(f, fmt.Sprint("exit:", v), nil)
		if code != v || len(out) != 0 || len(err) != 0 {
			t.Fatalf("exit %d: code=%d out=%q diag=%q", v, code, out, err)
		}
	}
	text := "中文\n'$HOME' `literal`"
	code, out, err := runFixture(f, text, nil)
	if code != 0 || string(out) != text || len(err) > 0 {
		t.Fatal("raw command/output changed")
	}
}
func TestBinaryInputAndDefaultEOF(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(t.TempDir(), "input")
	data := []byte{0, 255, 10, 1, 2}
	os.WriteFile(p, data, 0600)
	code, out, err := runFixture(f, "cat", nil, "--stdin", p)
	if code != 0 || !bytes.Equal(out, data) || len(err) > 0 {
		t.Fatalf("stdin failed %d %q", code, err)
	}
	code, out, _ = runFixture(f, "cat", nil)
	if code != 0 || len(out) != 0 {
		t.Fatal("default stdin not EOF")
	}
}
func TestOpenStdinDoesNotBlockExitOrCancel(t *testing.T) {
	f := newFixture(t)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	for _, cmd := range []string{"ok", "no-ack", "hang"} {
		start := time.Now()
		code, _, _ := runFixture(f, cmd, r, "--stdin", "-", "--timeout", "60ms")
		if time.Since(start) > time.Second {
			t.Fatal("blocked stdin leaked")
		}
		if cmd == "ok" && code != 0 {
			t.Fatal("exit waits for stdin")
		}
		if cmd != "ok" && code != 124 {
			t.Fatalf("timeout got %d", code)
		}
	}
}
func TestOutputLimitDrainsAndSinkFailureCloses(t *testing.T) {
	f := newFixture(t)
	code, out, err := runFixture(f, "out:20971520", nil, "--max-output", "1024")
	if code != 0 || len(out) != 1024 || !bytes.Contains(err, []byte("20970496")) {
		t.Fatalf("drain/truncation failed %d %d %q", code, len(out), err)
	}
	var diag bytes.Buffer
	code = Run(context.Background(), []string{"exec", "test", "-F", f.config, "--command", "out:20971520"}, nil, errorWriter{}, &diag, "test")
	if code != 125 {
		t.Fatalf("output failure: %d", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }
func TestBlockedOutputRespectsTimeout(t *testing.T) {
	f := newFixture(t)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	var diag bytes.Buffer
	start := time.Now()
	code := Run(context.Background(), []string{"exec", "test", "-F", f.config, "--command", "out:20971520", "--max-output", "0", "--timeout", "100ms"}, nil, w, &diag, "test")
	if code != 124 || time.Since(start) > time.Second {
		t.Fatalf("blocked output: %d %s", code, time.Since(start))
	}
}
func TestHandshakeDeadlineAndCancellation(t *testing.T) {
	f := newFixture(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := l.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	cfg, _ := loadConfig(f.config)
	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := strconv.Atoi(port)
	s := cfg.Servers["test"]
	s.Host = host
	s.Port = p
	cfg.Servers["test"] = s
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	b, e := connect(ctx, cfg, "test")
	if b != nil {
		b.Close()
	}
	if e == nil || time.Since(start) > time.Second {
		t.Fatal("handshake cancellation failed")
	}
	<-done
}
func TestHostKeyAndAuthenticationFailuresAreRedacted(t *testing.T) {
	f := newFixture(t)
	raw, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, bytes.ReplaceAll(raw, []byte(f.password), []byte("wrong-secret")), 0600)
	code, _, diag := runFixture(f, "ok", nil)
	if code != 125 || bytes.Contains(diag, []byte("wrong-secret")) {
		t.Fatal("authentication failed incorrectly")
	}
	os.WriteFile(f.config, raw, 0600)
	cfg, _ := loadConfig(f.config)
	os.WriteFile(cfg.KnownHosts, []byte(knownhosts.Line([]string{cfg.Servers["test"].address()}, testHostKey(t))+"\n"), 0600)
	code, _, diag = runFixture(f, "ok", nil)
	if code != 125 || !bytes.Contains(diag, []byte("SHA256:")) {
		t.Fatal("changed host key was accepted")
	}
}
func TestConfigurationValidationAndSecretRedaction(t *testing.T) {
	f := newFixture(t)
	raw, _ := os.ReadFile(f.config)
	for _, suffix := range []string{"\nunknown = 'sensitive-value'\n", "\nproxy_jump = 'missing'\n", "\nproxy_jump = 'test'\n", "\nmode = 'readonly'\n"} {
		os.WriteFile(f.config, append(append([]byte{}, raw...), []byte(suffix)...), 0600)
		_, e := loadConfig(f.config)
		if e == nil || strings.Contains(e.Error(), "sensitive-value") {
			t.Fatal("invalid config accepted/leaked")
		}
	}
	os.WriteFile(f.config, raw, 0600)
	os.Chmod(f.config, 0644)
	if _, e := loadConfig(f.config); e == nil {
		t.Fatal("world-readable credentials accepted")
	}
}
func TestMissingExitStatusIsUncertain(t *testing.T) {
	f := newFixture(t)
	code, _, diag := runFixture(f, "drop", nil)
	if code != 125 || !bytes.Contains(diag, []byte("远端可能已执行")) {
		t.Fatal("missing exit status misclassified")
	}
}

func TestHostsDetailsDoNotExposeCredentials(t *testing.T) {
	f := newFixture(t)
	var out, diag bytes.Buffer
	code := Run(context.Background(), []string{"hosts", "-F", f.config, "--details"}, nil, &out, &diag, "test")
	if code != 0 || !strings.HasPrefix(out.String(), "test\t") || strings.Contains(out.String(), f.password) || strings.Contains(out.String(), "127.0.0.1") {
		t.Fatal("host discovery exposed connection credentials")
	}
}

func TestKnownHostsFIFORejectedWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKnownHosts(p); err == nil {
		t.Fatal("known_hosts FIFO accepted")
	}
}
func TestParallelCancellationIsolation(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	failures := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd, ttl, expected := "ok", "3s", 0
			if i%2 == 0 {
				cmd, ttl, expected = "hang", "40ms", 124
			}
			code, _, _ := runFixture(f, cmd, nil, "--timeout", ttl)
			if code != expected {
				failures <- fmt.Sprint(i, ":", code)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Error(e)
	}
}
func TestCancellationRegistrationRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		b := &connection{}
		r, w, _ := os.Pipe()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); b.own(r) }()
		go func() { defer wg.Done(); b.Close() }()
		wg.Wait()
		if _, e := r.Read(make([]byte, 1)); !errors.Is(e, os.ErrClosed) {
			t.Fatal("resource survived close")
		}
		w.Close()
	}
}

func fdCount() int {
	for _, p := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(p); err == nil {
			return len(entries)
		}
	}
	var lim unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &lim) != nil {
		return -1
	}
	n := 0
	for fd := uint64(0); fd < min(lim.Cur, 65536); fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}

func TestSoakConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("1000-connection soak")
	}
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		runFixture(f, "warm", nil)
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	beforeFD, beforeG := fdCount(), runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var mu sync.Mutex
	latencies := []float64{}
	failed := atomic.Int64{}
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				cmd, ttl, expected := "soak", "3s", 0
				if i%10 == 0 {
					cmd, ttl, expected = "no-ack", "20ms", 124
				}
				s := time.Now()
				code, _, _ := runFixture(f, cmd, nil, "--timeout", ttl)
				if code != expected {
					failed.Add(1)
				}
				mu.Lock()
				latencies = append(latencies, float64(time.Since(s).Microseconds())/1000)
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	afterFD, afterG := fdCount(), runtime.NumGoroutine()
	sort.Float64s(latencies)
	report := map[string]any{"calls": 1000, "workers": 8, "failures": failed.Load(), "elapsed_seconds": elapsed.Seconds(), "p50_ms": latencies[500], "p95_ms": latencies[950], "p99_ms": latencies[990], "fd_before": beforeFD, "fd_after": afterFD, "goroutines_before": beforeG, "goroutines_after": afterG, "heap_before": before.HeapAlloc, "heap_after": after.HeapAlloc, "active_server_connections": f.active.Load()}
	data, _ := json.Marshal(report)
	t.Log(string(data))
	if failed.Load() != 0 || afterG > beforeG+4 || beforeFD < 0 || afterFD > beforeFD+2 || f.active.Load() != 0 || after.HeapAlloc > before.HeapAlloc+8<<20 {
		t.Fatal("soak resources did not return to baseline")
	}
}
