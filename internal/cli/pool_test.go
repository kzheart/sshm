package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh/knownhosts"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestPoolThousandCalls(t *testing.T) {
	f := newFixture(t)
	p := newPool(time.Minute)
	defer p.close()
	runtime.GC()
	beforeFD, beforeG := fdCount(), runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < 1000; i += 16 {
				var out, diag bytes.Buffer
				command := fmt.Sprint(i)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				code := executeWithPool(ctx, execOptions{config: f.config, host: "test", command: command, maxOutput: 1024}, nil, &out, &diag, p)
				cancel()
				if code != 0 || out.String() != command || diag.Len() != 0 {
					t.Errorf("call %d: code=%d out=%q diag=%s", i, code, out.String(), diag.String())
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	if f.active.Load() != 1 {
		t.Fatalf("expected exactly one shared transport, got %d", f.active.Load())
	}
	p.mu.Lock()
	if len(p.entries) != 1 {
		t.Errorf("entries=%d", len(p.entries))
	}
	for _, e := range p.entries {
		if e.active != 0 {
			t.Errorf("active sessions=%d", e.active)
		}
	}
	p.mu.Unlock()
	p.close()
	deadline := time.Now().Add(time.Second)
	for f.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.active.Load() != 0 {
		t.Fatal("transport was not closed")
	}
	elapsed := time.Since(start)
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	afterFD, afterG := fdCount(), runtime.NumGoroutine()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	record := map[string]any{"calls": 1000, "workers": 16, "transports": 1, "seconds": elapsed.Seconds(), "fd_before": beforeFD, "fd_after": afterFD, "goroutines_before": beforeG, "goroutines_after": afterG, "heap_before": before.HeapAlloc, "heap_after": after.HeapAlloc, "connections_after": f.active.Load()}
	data, _ := json.Marshal(record)
	t.Log(string(data))
	if afterFD > beforeFD+2 || afterG > beforeG+2 || after.HeapAlloc > before.HeapAlloc+(8<<20) {
		t.Fatal("pooled resources failed to return to baseline")
	}
}

func TestPoolRetirementAndIdle(t *testing.T) {
	f := newFixture(t)
	cfg, err := loadConfig(f.config)
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(20 * time.Millisecond)
	defer p.close()
	b, release, reused, err := p.acquire(context.Background(), cfg, "test", f.config)
	if err != nil || reused {
		t.Fatalf("first acquisition: %v %v", reused, err)
	}
	b2, release2, reused, err := p.acquire(context.Background(), cfg, "test", f.config)
	if err != nil || !reused || b2 != b {
		t.Fatal("not shared")
	}
	release(true)
	s, err := b2.client.NewSession()
	if err != nil {
		t.Fatal("cancel interrupted sibling", err)
	}
	out, err := s.Output("sibling")
	s.Close()
	if err != nil || string(out) != "sibling" {
		t.Fatal("sibling failed", err)
	}
	release2(false)
	b3, release3, reused, err := p.acquire(context.Background(), cfg, "test", f.config)
	if err != nil || reused || b3 == b {
		t.Fatal("retired connection reused")
	}
	release3(false)
	p.sweep(time.Now().Add(time.Second))
	p.mu.Lock()
	count := len(p.entries)
	p.mu.Unlock()
	if count != 0 {
		t.Fatal("idle connection retained")
	}
}

func TestPoolRevalidatesTrustAndCredentials(t *testing.T) {
	f := newFixture(t)
	cfg, _ := loadConfig(f.config)
	p := newPool(time.Minute)
	defer p.close()
	_, release, _, err := p.acquire(context.Background(), cfg, "test", f.config)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	known, _ := os.ReadFile(cfg.KnownHosts)
	os.WriteFile(cfg.KnownHosts, append(known, []byte("# updated\n")...), 0600)
	_, release, reused, err := p.acquire(context.Background(), cfg, "test", f.config)
	if err != nil || reused {
		t.Fatal("trust update did not invalidate cached connection", err)
	}
	release(false)
	s := cfg.Servers["test"]
	s.Password = "invalid-synthetic-password"
	cfg.Servers["test"] = s
	if _, _, _, err = p.acquire(context.Background(), cfg, "test", f.config); err == nil {
		t.Fatal("credential update used previous authentication")
	}
	os.WriteFile(cfg.KnownHosts, []byte(knownhosts.Line([]string{cfg.Servers["test"].address()}, testHostKey(t))+"\n"), 0600)
	s.Password = f.password
	cfg.Servers["test"] = s
	if _, _, _, err = p.acquire(context.Background(), cfg, "test", f.config); err == nil {
		t.Fatal("changed trusted key still accepted")
	}
}

func TestPoolQueueDeadline(t *testing.T) {
	f := newFixture(t)
	cfg, _ := loadConfig(f.config)
	p := newPool(time.Minute)
	defer p.close()
	for i := 0; i < poolSessions; i++ {
		_, release, _, err := p.acquire(context.Background(), cfg, "test", f.config)
		if err != nil {
			t.Fatal(err)
		}
		defer release(false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, _, err := p.acquire(ctx, cfg, "test", f.config); err != context.DeadlineExceeded {
		t.Fatal("queue did not honor deadline", err)
	}
}

func TestPoolHostLimit(t *testing.T) {
	f := newFixture(t)
	cfg, _ := loadConfig(f.config)
	p := newPool(time.Minute)
	defer p.close()
	var first func(bool)
	for i := 0; i <= poolHosts; i++ {
		cfg.Servers[fmt.Sprint(i)] = cfg.Servers["test"]
	}
	for i := 0; i < poolHosts; i++ {
		_, release, _, err := p.acquire(context.Background(), cfg, fmt.Sprint(i), f.config)
		if err != nil {
			t.Fatal(err)
		}
		defer release(false)
		if i == 0 {
			first = release
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, _, err := p.acquire(ctx, cfg, fmt.Sprint(poolHosts), f.config); err != context.DeadlineExceeded {
		t.Fatal("host limit did not queue with deadline", err)
	}
	first(false)
	_, release, _, err := p.acquire(context.Background(), cfg, fmt.Sprint(poolHosts), f.config)
	if err != nil {
		t.Fatal("idle connection was not evicted", err)
	}
	release(false)
}
