package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const poolIdle = 60 * time.Second
const poolHosts = 32
const poolSessions = 8 // Below the common sshd MaxSessions=10 default.

type poolEntry struct {
	key     [32]byte
	b       *connection
	ready   chan struct{}
	active  int
	last    time.Time
	retired bool
}

type connectionPool struct {
	mu      sync.Mutex
	entries map[string]*poolEntry
	changed chan struct{}
	closed  bool
	idle    time.Duration
}

func newPool(idle time.Duration) *connectionPool {
	return &connectionPool{entries: make(map[string]*poolEntry), changed: make(chan struct{}), idle: idle}
}
func (p *connectionPool) notify() { close(p.changed); p.changed = make(chan struct{}) }

// Revalidate file ownership/permissions and hash actual trust/key contents on
// every invocation. No credentials, fingerprints of secrets, or commands on disk.
func connectionKey(cfg *configuration, alias string) ([32]byte, error) {
	h := sha256.New()
	names, err := cfg.chain(alias)
	if err != nil {
		return [32]byte{}, err
	}
	known, err := knownHostsContents(cfg.KnownHosts)
	if err != nil {
		return [32]byte{}, err
	}
	enc := json.NewEncoder(h)
	_ = enc.Encode(known)
	for _, name := range names {
		s := cfg.Servers[name]
		_ = enc.Encode(s)
		if s.KeyPath != "" {
			key, err := privateRead(s.KeyPath, 1<<20, true)
			if err != nil {
				return [32]byte{}, err
			}
			_ = enc.Encode(key)
		}
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key, nil
}

func (p *connectionPool) acquire(ctx context.Context, cfg *configuration, alias, path string) (*connection, func(bool), bool, error) {
	key, err := connectionKey(cfg, alias)
	if err != nil {
		return nil, nil, false, err
	}
	id := path + "\x00" + alias
	for {
		if ctx.Err() != nil {
			return nil, nil, false, ctx.Err()
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, false, errors.New("连接服务已关闭")
		}
		e := p.entries[id]
		if e != nil && e.key != key {
			e.retired = true
			delete(p.entries, id)
			if e.active == 0 && e.b != nil {
				e.b.Close()
			}
			e = nil
		}
		if e == nil {
			if len(p.entries) >= poolHosts {
				// Evict an idle connection, never interrupt an active command.
				for oldID, old := range p.entries {
					if old.active == 0 {
						old.retired = true
						old.b.Close()
						delete(p.entries, oldID)
						break
					}
				}
				if len(p.entries) >= poolHosts {
					wake := p.changed
					p.mu.Unlock()
					select {
					case <-ctx.Done():
						return nil, nil, false, ctx.Err()
					case <-wake:
						continue
					}
				}
			}
			e = &poolEntry{key: key, ready: make(chan struct{}), active: 1}
			p.entries[id] = e
			p.mu.Unlock()
			b, err := connect(ctx, cfg, alias)
			p.mu.Lock()
			e.b = b
			if err != nil || p.closed || e.retired {
				if p.entries[id] == e {
					delete(p.entries, id)
				}
				e.retired = true
				if b != nil {
					b.Close()
				}
				if err == nil {
					err = errors.New("连接配置已更新或服务已关闭")
				}
			}
			close(e.ready)
			p.notify()
			p.mu.Unlock()
			if err != nil {
				return nil, nil, false, err
			}
			go p.watch(id, e)
			return b, p.releaser(id, e), false, nil
		}
		select {
		case <-e.ready:
			if e.active < poolSessions {
				e.active++
				p.mu.Unlock()
				return e.b, p.releaser(id, e), true, nil
			}
		default:
		}
		wake := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		case <-wake:
		}
	}
}

// Closing an SSH channel does not force the peer to acknowledge closure. A
// cancelled lease retires the transport; healthy siblings can finish, then the
// transport is closed even if the peer kept an abandoned command/channel open.
func (p *connectionPool) releaser(id string, e *poolEntry) func(bool) {
	var once sync.Once
	return func(retire bool) {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if retire {
				e.retired = true
				if p.entries[id] == e {
					delete(p.entries, id)
				}
			}
			e.active--
			e.last = time.Now()
			if e.active == 0 && (e.retired || p.closed) {
				e.b.Close()
			}
			p.notify()
		})
	}
}

func (p *connectionPool) watch(id string, e *poolEntry) {
	// A bounded liveness probe also releases blocked channel-open/request writes
	// after silent network loss. Only transport failure closes sibling channels.
	done := make(chan struct{})
	go func() { e.b.client.Wait(); close(done) }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer func() {
		e.b.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		e.retired = true
		if p.entries[id] == e {
			delete(p.entries, id)
		}
		p.notify()
	}()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			probe := make(chan error, 1)
			go func() { _, _, err := e.b.client.SendRequest("keepalive@openssh.com", true, nil); probe <- err }()
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-done:
				timer.Stop()
				return
			case err := <-probe:
				timer.Stop()
				if err != nil {
					return
				}
			case <-timer.C:
				return
			}
		}
	}
}

func (p *connectionPool) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.entries {
		if e.active == 0 && now.Sub(e.last) >= p.idle {
			e.retired = true
			e.b.Close()
			delete(p.entries, id)
		}
	}
	p.notify()
}
func (p *connectionPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, e := range p.entries {
		if e.b != nil {
			e.b.Close()
		}
	}
	p.notify()
}
