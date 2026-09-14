package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
)

type server struct {
	Host        string `toml:"host"`
	User        string `toml:"user"`
	Port        int    `toml:"port"`
	Password    string `toml:"password"`
	KeyPath     string `toml:"key_path"`
	Passphrase  string `toml:"passphrase"`
	ProxyJump   string `toml:"proxy_jump"`
	Description string `toml:"description"`
	DefaultDir  string `toml:"default_dir"`
	Mode        string `toml:"mode"`
}
type configuration struct {
	KnownHosts string            `toml:"known_hosts"`
	Servers    map[string]server `toml:"ssh_servers"`
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// Configuration and private key input cannot be devices/FIFOs or unbounded.
func privateRead(path string, limit int64, private bool) ([]byte, error) {
	f, err := os.OpenFile(expand(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("无法打开本地文件")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("必须使用普通文件")
	}
	if private {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("文件必须属于当前用户且仅当前用户可读写（chmod 600）")
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("无法读取文件或文件超过大小限制")
	}
	return b, nil
}

func loadConfig(path string) (*configuration, error) {
	b, err := privateRead(path, 4<<20, true)
	if err != nil {
		return nil, fmt.Errorf("TOML 配置：%w", err)
	}
	var cfg configuration
	// Decoder errors can contain source lines, including passwords. Never echo them.
	if err := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&cfg); err != nil {
		return nil, errors.New("TOML 格式或字段无效；仅支持 known_hosts 和 ssh_servers 下的连接字段")
	}
	if len(cfg.Servers) == 0 {
		return nil, errors.New("配置中没有 ssh_servers")
	}
	if cfg.KnownHosts == "" {
		cfg.KnownHosts = "~/.ssh/known_hosts"
	}
	cfg.KnownHosts = expand(cfg.KnownHosts)
	if !filepath.IsAbs(cfg.KnownHosts) {
		cfg.KnownHosts = filepath.Join(filepath.Dir(path), cfg.KnownHosts)
	}
	for alias, s := range cfg.Servers {
		if !validHost(alias) || s.Host == "" || strings.ContainsAny(s.Host, "\x00\r\n\t /\\") || s.User == "" || strings.ContainsAny(s.User, "\x00\r\n") {
			return nil, errors.New("服务器别名、host 或 user 无效")
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if s.Port < 1 || s.Port > 65535 {
			return nil, fmt.Errorf("%s：port 超出范围", alias)
		}
		if s.Mode != "" && s.Mode != "unrestricted" {
			return nil, fmt.Errorf("%s：不支持旧 MCP 的权限 mode；不能静默忽略该限制", alias)
		}
		if s.KeyPath != "" {
			s.KeyPath = expand(s.KeyPath)
			if !filepath.IsAbs(s.KeyPath) {
				s.KeyPath = filepath.Join(filepath.Dir(path), s.KeyPath)
			}
		}
		cfg.Servers[alias] = s
	}
	for alias := range cfg.Servers {
		if _, err := cfg.chain(alias); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

func (c *configuration) chain(alias string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for alias != "" {
		s, ok := c.Servers[alias]
		if !ok {
			return nil, fmt.Errorf("未配置的服务器或跳板别名：%s", alias)
		}
		if seen[alias] || len(out) >= 8 {
			return nil, errors.New("ProxyJump 循环或超过 8 层")
		}
		seen[alias] = true
		out = append(out, alias)
		alias = s.ProxyJump
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s server) address() string {
	return net.JoinHostPort(strings.Trim(s.Host, "[]"), fmt.Sprint(s.Port))
}

func hostsCommand(args []string, stdout, stderr io.Writer) int {
	f := flags("hosts", "用法：sshm hosts [筛选文本] [-F TOML]；只列别名，不连接服务器。", stderr)
	path := configFlag(f)
	details := f.Bool("details", false, "显示描述与默认目录提示，不显示地址或凭据")
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() > 1 {
		return parseError(errors.New("只接受一个筛选文本"), stderr)
	}
	p, err := configPath(*path)
	if err != nil {
		return parseError(err, stderr)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		return localFailure(nil, stderr, err, false)
	}
	names := []string{}
	for n := range cfg.Servers {
		if strings.Contains(strings.ToLower(n), strings.ToLower(f.Arg(0))) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if *details {
			s := cfg.Servers[n]
			clean := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", n, clean.Replace(s.Description), clean.Replace(s.DefaultDir))
		} else {
			fmt.Fprintln(stdout, n)
		}
	}
	return 0
}
