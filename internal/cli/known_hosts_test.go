package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestFirstConnectionRecordsHostKey(t *testing.T) {
	for _, missing := range []bool{false, true} {
		f := newFixture(t)
		cfg, _ := loadConfig(f.config)
		if missing {
			os.Remove(cfg.KnownHosts)
		} else {
			os.WriteFile(cfg.KnownHosts, []byte("# preserve without newline"), 0600)
		}
		p := newPool(time.Minute)
		_, release, _, err := p.acquire(context.Background(), cfg, "test", f.config)
		if err != nil {
			p.close()
			t.Fatal(err)
		}
		release(false)
		p.close()
		data, err := os.ReadFile(cfg.KnownHosts)
		if err != nil || !bytes.Contains(data, []byte("ssh-ed25519")) {
			t.Fatalf("key not recorded: %v", err)
		}
		if !missing && !bytes.HasPrefix(data, []byte("# preserve without newline\n")) {
			t.Fatal("existing contents changed")
		}
		if missing {
			info, _ := os.Stat(cfg.KnownHosts)
			if info.Mode().Perm() != 0600 {
				t.Fatal("incorrect file permissions")
			}
		}
		code, out, diag := runFixture(f, "connected", nil)
		if code != 0 || string(out) != "connected" || len(diag) != 0 {
			t.Fatalf("reconnect: %d %s", code, diag)
		}
		after, _ := os.ReadFile(cfg.KnownHosts)
		if !bytes.Equal(data, after) {
			t.Fatal("reconnect changed trust file")
		}
	}
}

func TestConcurrentFirstHostKeys(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "nested", "known_hosts")
		keys := []ssh.PublicKey{testHostKey(t), testHostKey(t)}
		if !conflicting {
			keys[1] = keys[0]
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range keys {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = verifyHostKey(context.Background(), path, "example.test:2222", &net.TCPAddr{}, keys[i])
			}(i)
		}
		wg.Wait()
		success := 0
		for _, err := range errs {
			if err == nil {
				success++
			}
		}
		want := 2
		if conflicting {
			want = 1
		}
		if success != want {
			t.Fatalf("success=%d errors=%v", success, errs)
		}
		data, _ := os.ReadFile(path)
		if strings.Count(string(data), "ssh-ed25519") != 1 {
			t.Fatal("duplicate or conflicting keys recorded")
		}
	}
}

func TestHostKeyRejectsRevokedMalformedAndCancelledWrites(t *testing.T) {
	key := testHostKey(t)
	for _, contents := range []string{"invalid known hosts line\n", "@revoked " + knownhosts.Line([]string{"example.test:22"}, key) + "\n"} {
		path := filepath.Join(t.TempDir(), "known_hosts")
		os.WriteFile(path, []byte(contents), 0600)
		if verifyHostKey(context.Background(), path, "example.test:22", &net.TCPAddr{}, key) == nil {
			t.Fatal("invalid trust accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != contents {
			t.Fatal("invalid trust overwritten")
		}
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if verifyHostKey(ctx, path, "example.test:22", &net.TCPAddr{}, key) == nil || time.Since(start) > time.Second {
		t.Fatal("lock wait ignored deadline")
	}
	data, _ := os.ReadFile(path)
	if len(data) != 0 {
		t.Fatal("cancelled write recorded key")
	}
}
