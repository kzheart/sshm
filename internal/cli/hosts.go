package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type configFile struct {
	path, includeBase string
	optional          bool
}

func configFiles(config string) ([]configFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".ssh")
	if config != "" {
		return []configFile{{path: config, includeBase: base}}, nil
	}
	return []configFile{
		{path: filepath.Join(base, "config"), includeBase: base, optional: true},
		{path: "/etc/ssh/ssh_config", includeBase: "/etc/ssh", optional: true},
	}, nil
}

type discovery struct {
	hosts    map[string]string
	stack    map[string]bool
	warnings []string
	files    int
}

func discover(files []configFile) ([]string, []string, error) {
	d := &discovery{hosts: map[string]string{}, stack: map[string]bool{}}
	for _, f := range files {
		if err := d.read(f, 0); err != nil {
			return nil, d.warnings, err
		}
	}
	var hosts []string
	for _, h := range d.hosts {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return strings.ToLower(hosts[i]) < strings.ToLower(hosts[j]) })
	return hosts, d.warnings, nil
}

// Discovery is deliberately not an SSH config evaluator. Enumerate literal
// Host declarations (including conditional Include candidates), leaving actual
// matching and connection settings to OpenSSH when a destination is used.
func (d *discovery) read(cf configFile, depth int) error {
	if depth > 16 || d.files >= 1024 {
		return errors.New("SSH Include 超出发现限制（深度 16 / 文件数 1024）")
	}
	real, err := filepath.EvalSymlinks(cf.path)
	if err != nil {
		if cf.optional && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if d.stack[real] {
		return fmt.Errorf("SSH Include 循环：%s", cf.path)
	}
	d.stack[real] = true
	defer delete(d.stack, real)
	d.files++
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && real != os.DevNull {
		return fmt.Errorf("配置不是普通文件：%s", real)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		i := strings.IndexAny(s, " \t=")
		if i < 0 {
			continue
		}
		key, value := strings.ToLower(s[:i]), strings.TrimSpace(s[i:])
		value = strings.TrimSpace(strings.TrimPrefix(value, "="))
		if key != "host" && key != "include" {
			continue
		}
		words, err := configWords(value)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", cf.path, line, err)
		}
		if key == "host" {
			for _, h := range words {
				if validHost(h) && !strings.ContainsAny(h, "[]") {
					k := strings.ToLower(h)
					if _, exists := d.hosts[k]; !exists {
						d.hosts[k] = h
					}
				}
			}
			continue
		}
		for _, pattern := range words {
			pattern, err = includePattern(pattern, cf.includeBase)
			if err != nil {
				d.warnings = append(d.warnings, fmt.Sprintf("%s:%d: %v", cf.path, line, err))
				continue
			}
			matches, err := filepath.Glob(pattern)
			if err != nil {
				return fmt.Errorf("%s:%d: Include 路径错误：%w", cf.path, line, err)
			}
			for _, match := range matches {
				if err := d.read(configFile{path: match, includeBase: cf.includeBase}, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return scanner.Err()
}

func includePattern(p, base string) (string, error) {
	if strings.Contains(p, "%") {
		return "", fmt.Errorf("Include 含目标相关 token，未枚举：%s", p)
	}
	if strings.Contains(p, "${") {
		missing := ""
		p = os.Expand(p, func(key string) string {
			v, ok := os.LookupEnv(key)
			if !ok {
				missing = key
			}
			return v
		})
		if missing != "" {
			return "", fmt.Errorf("Include 环境变量未设置：%s", missing)
		}
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	} else if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("未枚举其他用户的 Include 路径：%s", p)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p, nil
}

// OpenSSH configuration accepts quoted words and backslash escapes. Only Host
// and Include values are tokenized; shell expressions in Match are never run.
func configWords(s string) ([]string, error) {
	var words []string
	var b strings.Builder
	var quote rune
	escape, word := false, false
	flush := func() {
		if word {
			words = append(words, b.String())
			b.Reset()
			word = false
		}
	}
	for _, r := range s {
		if escape {
			b.WriteRune(r)
			word, escape = true, false
			continue
		}
		if r == '\\' {
			escape, word = true, true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			continue
		}
		switch r {
		case '#':
			flush()
			return words, nil
		case '\'', '"':
			quote, word = r, true
		case ' ', '\t':
			flush()
		default:
			b.WriteRune(r)
			word = true
		}
	}
	if escape || quote != 0 {
		return nil, errors.New("未闭合的引号或转义")
	}
	flush()
	return words, nil
}

func hostsCommand(args []string, stdout, stderr io.Writer) int {
	f := flags("hosts", "用法：sshm hosts [筛选文本] [-F 配置文件]\n每行一个候选别名，不连接服务器。包含条件 Include 中的声明；实际是否匹配由 OpenSSH 决定。", stderr)
	cfg := configFlag(f)
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() > 1 {
		return parseError(errors.New("最多提供一个别名筛选文本"), stderr)
	}
	p, err := configPath(*cfg)
	if err != nil {
		return parseError(err, stderr)
	}
	files, err := configFiles(p)
	if err != nil {
		fmt.Fprintf(stderr, "sshm: 本地错误：%v\n", err)
		return 125
	}
	hosts, warnings, err := discover(files)
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "sshm: 发现提示：%s\n", warning)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sshm: 配置发现失败：%v\n", err)
		return 125
	}
	count := 0
	for _, h := range hosts {
		if strings.Contains(strings.ToLower(h), strings.ToLower(f.Arg(0))) {
			if _, err := fmt.Fprintln(stdout, h); err != nil {
				return 125
			}
			count++
		}
	}
	if count == 0 {
		fmt.Fprintln(stderr, "sshm: 未找到匹配的明确主机别名；通配符、DNS 和云主机不会自动展开。")
	}
	if len(warnings) > 0 {
		return 1 // An incomplete inventory must not silently look complete.
	}
	return 0
}
