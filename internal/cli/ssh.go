package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Every transport in a jump chain belongs to this invocation. A close racing
// with registration immediately closes the new resource, never loses it.
type connection struct {
	mu        sync.Mutex
	closed    bool
	resources []io.Closer
	client    *ssh.Client
}

func (b *connection) own(c io.Closer) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		c.Close()
		return
	}
	b.resources = append(b.resources, c)
	b.mu.Unlock()
}
func (b *connection) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	r := b.resources
	b.resources = nil
	b.mu.Unlock()
	for _, c := range r {
		_ = c.Close()
	}
	return nil
}

func authMethods(s server) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if s.KeyPath != "" {
		b, err := privateRead(s.KeyPath, 1<<20, true)
		if err != nil {
			return nil, fmt.Errorf("私钥：%w", err)
		}
		var signer ssh.Signer
		if s.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(b, []byte(s.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(b)
		}
		if err != nil {
			return nil, errors.New("无法解析私钥；检查格式与 passphrase")
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if s.Password != "" {
		methods = append(methods, ssh.Password(s.Password), ssh.KeyboardInteractive(func(_, _ string, questions []string, echo []bool) ([]string, error) {
			// Password-only keyboard-interactive. Never submit passwords as OTP answers.
			if len(questions) != 1 || len(echo) != 1 || echo[0] {
				return nil, errors.New("需要交互式多因素认证")
			}
			q := strings.ToLower(strings.TrimSpace(questions[0]))
			if q != "password:" && q != "password" {
				return nil, errors.New("不支持的认证问题")
			}
			return []string{s.Password}, nil
		}))
	}
	if len(methods) == 0 {
		return nil, errors.New("需要配置 password 或 key_path")
	}
	return methods, nil
}

// Probe against knownhosts to prefer algorithms for keys we actually trust.
// This is only algorithm selection; the real handshake is verified again.
type probeKey struct{}

func (probeKey) Type() string                        { return "sshm-probe" }
func (probeKey) Marshal() []byte                     { return nil }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe") }
func trustedAlgorithms(cb ssh.HostKeyCallback, addr string) []string {
	var ke *knownhosts.KeyError
	if !errors.As(cb(addr, &net.TCPAddr{}, probeKey{}), &ke) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, want := range ke.Want {
		types := []string{want.Key.Type()}
		if want.Key.Type() == ssh.KeyAlgoRSA {
			types = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}
		for _, t := range types {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

func connect(ctx context.Context, cfg *configuration, alias string) (*connection, error) {
	names, err := cfg.chain(alias)
	if err != nil {
		return nil, err
	}
	cb, err := readKnownHosts(cfg.KnownHosts)
	if err != nil {
		return nil, errors.New("无法读取 known_hosts；请提供有效的可信主机公钥文件")
	}
	b := &connection{}
	stop := context.AfterFunc(ctx, func() { b.Close() })
	// The caller maintains its own cancellation watcher after this function.
	defer stop()
	success := false
	defer func() {
		if !success {
			b.Close()
		}
	}()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := cfg.Servers[name]
		addr := s.address()
		auth, err := authMethods(s)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", name, err)
		}
		hopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var raw net.Conn
		if b.client == nil {
			raw, err = (&net.Dialer{}).DialContext(hopCtx, "tcp", addr)
		} else {
			raw, err = b.client.DialContext(hopCtx, "tcp", addr)
		}
		if err != nil {
			cancel()
			return nil, fmt.Errorf("%s：连接失败", name)
		}
		b.own(raw)
		watch := context.AfterFunc(hopCtx, func() { raw.Close() })
		var trustErr error
		clientCfg := &ssh.ClientConfig{User: s.User, Auth: auth, HostKeyAlgorithms: trustedAlgorithms(cb, addr), HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			err := cb(host, remote, key)
			if err != nil {
				trustErr = fmt.Errorf("%s：主机公钥未受信任或已变化（%s）；请独立核对并更新 known_hosts", name, ssh.FingerprintSHA256(key))
			}
			return err
		}}
		cc, chans, reqs, handshakeErr := ssh.NewClientConn(raw, addr, clientCfg)
		watch()
		hopErr := hopCtx.Err()
		cancel()
		if handshakeErr != nil || hopErr != nil {
			if cc != nil {
				cc.Close()
			}
			if trustErr != nil {
				return nil, trustErr
			}
			return nil, fmt.Errorf("%s：SSH 握手或认证失败；检查凭据、算法兼容性及连接期限", name)
		}
		client := ssh.NewClient(cc, chans, reqs)
		b.own(client)
		b.client = client
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	success = true
	return b, nil
}

func readKnownHosts(path string) (ssh.HostKeyCallback, error) {
	if _, err := privateRead(path, 8<<20, false); err != nil {
		return nil, err
	}
	return knownhosts.New(path)
}

func localFailure(ctx context.Context, out io.Writer, err error, started bool) int {
	state := "远端命令尚未请求执行。"
	if started {
		state = "远端可能已执行；不保证远端进程已停止，请先核实状态。"
	}
	code := 125
	kind := "本地/连接失败"
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = 124
			kind = "超时"
		} else {
			code = 130
			kind = "已取消"
		}
	}
	fmt.Fprintf(out, "sshm: %s：%v。%s\n", kind, err, state)
	return code
}

func execute(ctx context.Context, o execOptions, input *os.File, stdout, stderr io.Writer) int {
	started := time.Now()
	cfg, err := loadConfig(o.config)
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	b, err := connect(ctx, cfg, o.host)
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	defer b.Close()
	stop := context.AfterFunc(ctx, func() { b.Close() })
	defer stop()
	connected := time.Now()
	if o.debug {
		fmt.Fprintf(stderr, "sshm: connect_ms=%.3f\n", float64(connected.Sub(started).Microseconds())/1000)
	}
	s, err := b.client.NewSession()
	if err != nil {
		return localFailure(ctx, stderr, errors.New("无法建立命令通道"), false)
	}
	defer s.Close()
	out := &limitedWriter{out: contextOutput(ctx, stdout), limit: o.maxOutput, abort: func() { b.Close() }}
	diag := &limitedWriter{out: contextOutput(ctx, stderr), limit: o.maxOutput, abort: func() { b.Close() }}
	s.Stdout = out
	s.Stderr = diag
	in, err := s.StdinPipe()
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	// Mark uncertainty before the request, including a lost acknowledgement.
	if err := s.Start(o.command); err != nil {
		return localFailure(ctx, stderr, errors.New("执行请求未获确认"), true)
	}
	inputCtx, cancelInput := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer in.Close()
		if input != nil {
			_, _ = io.Copy(in, &contextReader{ctx: inputCtx, file: input})
		}
	}()
	err = s.Wait()
	cancelInput()
	b.Close()
	<-done
	if o.debug {
		fmt.Fprintf(stderr, "sshm: command_ms=%.3f total_ms=%.3f\n", float64(time.Since(connected).Microseconds())/1000, float64(time.Since(started).Microseconds())/1000)
	}
	if ctx.Err() != nil {
		return localFailure(ctx, stderr, ctx.Err(), true)
	}
	if out.err != nil || diag.err != nil {
		return localFailure(ctx, stderr, errors.New("本地输出写入失败"), true)
	}
	if out.dropped > 0 {
		fmt.Fprintf(stderr, "\nsshm: stdout 已截断，省略 %d 字节。\n", out.dropped)
	}
	if diag.dropped > 0 {
		fmt.Fprintf(stderr, "\nsshm: stderr 已截断，省略 %d 字节。\n", diag.dropped)
	}
	if err == nil {
		return 0
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) && exit.ExitStatus() >= 0 {
		return exit.ExitStatus()
	}
	return localFailure(ctx, stderr, errors.New("连接结束但未收到有效退出码"), true)
}
