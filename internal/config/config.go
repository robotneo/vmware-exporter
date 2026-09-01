// Package config 接管命令行、配置文件与环境变量三来源的 flag 解析。
//
// 这部分逻辑此前来自 prezhdarov/prometheus-exporter/pkg/config。随框架一并
// 移除时不能顺手删掉：-file 与 -envflag.* 是仓库对外承诺的接口，
// docker-compose.yml、vmware.conf 与两份 README 都在用它们，而 docker-compose
// 里正是靠 -envflag.enable 把密码从 command: 里挪出去，避免同主机上任何能读
// /proc 的进程看到明文。删掉它等于把一个已经修好的泄露口重新打开。
//
// 相对框架版本的两处实质改动，都是把静默失败变成明确失败：
//
//  1. Parse 返回 error 而不是 log.Fatalf。框架在库代码里直接终止进程，
//     调用方无从插手，测试也没法覆盖失败分支。
//  2. SetLogger 检查 promslog 的 Set 返回值。框架把它丢掉了，于是
//     -log.level=verbose 这种拼错的值会被静默降级成默认等级 ——
//     启动看起来正常，实际拿到的不是你要的日志级别。
package config

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/prometheus/common/promslog"
	"gopkg.in/yaml.v3"
)

var (
	enable = flag.Bool("envflag.enable", false, "Whether to enable reading flags from environment variables additionally to command line. "+
		"Command line flag and file values (if -file is set) have priority over values from environment vars. "+
		"Flags are read only from command line if this flag isn't set.")
	prefix = flag.String("envflag.prefix", "", "Prefix for environment variables if -envflag.enable is set")

	file = flag.String("file", "", "Path to file with configuration data.")
)

// Parse 解析 flag，优先级由高到低：命令行 > 配置文件 > 环境变量。
//
// 这个顺序不是任选的：命令行是运维当场的显式意图，配置文件是这台机器的
// 固化配置，环境变量是容器编排注入的兜底。反过来的话，临时用命令行覆盖
// 一个参数去排查问题就做不到了。
func Parse() error {
	flag.Parse()

	return applyFileAndEnv(flag.CommandLine, *file, *enable)
}

// applyFileAndEnv 是 Parse 的可测内核：把「读文件、读环境变量、按优先级
// 填入」这段逻辑与全局 flag.CommandLine 解耦。
//
// 拆出来的理由是测试无法安全地驱动 Parse()：flag.Parse() 读的是真实的
// os.Args，而 flag.CommandLine 是测试进程自己的 flag 集（-test.v 等都在
// 里面），往里注入会污染测试框架本身。传入 *flag.FlagSet 之后，每个测试
// 用一个独立的 FlagSet，互不干扰。
//
// fs 必须已经 Parse 过：这个函数依赖 fs.Visit 判断「哪些是命令行显式给的」，
// 而 Visit 在 Parse 之前返回空集，那样命令行优先级就失效了。
func applyFileAndEnv(fs *flag.FlagSet, path string, envEnabled bool) error {
	// 已在命令行出现过的 flag 不再被后两个来源覆盖。用 fs.Visit（只遍历
	// 被显式设置过的）而非 VisitAll，这正是「命令行优先」的实现方式。
	flagsSet := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		flagsSet[f.Name] = true
	})

	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read config file %s: %w", path, err)
		}

		var fileFlags map[string]string
		if err := yaml.Unmarshal(content, &fileFlags); err != nil {
			return fmt.Errorf("cannot parse config file %s: %w", path, err)
		}

		// 排序后再遍历：map 的迭代顺序是随机的，若文件里有两个坏 flag，
		// 不排序会导致每次报出的是随机的那一个，错误信息不可复现。
		names := make([]string, 0, len(fileFlags))
		for name := range fileFlags {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			if flagsSet[name] {
				continue
			}
			// 文件里写了一个不存在的 flag 名，说明配置写错了或者是升级后
			// 残留的旧名字。框架的实现会静默忽略 —— 于是「我明明配了
			// 超时」和「超时没生效」之间毫无线索。
			if fs.Lookup(name) == nil {
				return fmt.Errorf("config file %s sets unknown flag %q", path, name)
			}
			if err := fs.Set(name, fileFlags[name]); err != nil {
				return fmt.Errorf("cannot set flag %s to %q from %s: %w", name, fileFlags[name], path, err)
			}
			flagsSet[name] = true
		}
	}

	if !envEnabled {
		return nil
	}

	// 环境变量是最后一层：只填前两层没管过的 flag。
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if envErr != nil || flagsSet[f.Name] {
			return
		}
		name := EnvFlagName(f.Name)
		v, ok := os.LookupEnv(name)
		if !ok {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			envErr = fmt.Errorf("cannot set flag %s to %q from environment variable %s: %w", f.Name, v, name, err)
		}
	})

	return envErr
}

// EnvFlagName 把 flag 名映射成环境变量名。
//
// 导出是为了让 scripts/check_config.py 之外的调用方（以及测试）能引用
// 同一份规则，而不是各自复刻一遍字符串替换。
//
// 注意大小写：flag 名原样保留，不转大写。-vmware.password 对应的是
// VMWARE_vmware_password 而不是 VMWARE_VMWARE_PASSWORD。这条规则反直觉，
// 已经在两份 README 和 check_config.py 里各自写明过 —— 拼错了不会报错，
// 只是环境变量被无声忽略。
func EnvFlagName(s string) string {
	return *prefix + strings.ReplaceAll(s, ".", "_")
}

// SetLogger 由 -log.format 与 -log.level 构造 promslog 配置。
//
// 与框架版本的差别是这里检查 Set 的返回值。promslog 的 Set 在拿到非法值时
// 返回 error 并保持原值不变，框架把 error 丢了，结果 -log.level=verbose
// 会静默变成默认等级。日志级别配错本身不致命，但排查问题时你会盯着一个
// 「为什么没有 debug 日志」的假象浪费时间。
func SetLogger(format, level *string) (*promslog.Config, error) {
	promlogFormat := promslog.NewFormat()
	if err := promlogFormat.Set(*format); err != nil {
		return nil, fmt.Errorf("invalid -log.format %q: %w", *format, err)
	}

	promlogLevel := promslog.NewLevel()
	if err := promlogLevel.Set(*level); err != nil {
		return nil, fmt.Errorf("invalid -log.level %q: %w", *level, err)
	}

	return &promslog.Config{Format: promlogFormat, Level: promlogLevel}, nil
}

// Usage 打印 usage 文本与全部 flag 的默认值。
//
// 框架版本只在命令行带 -h/-help 时才打印默认值，否则提示「请加 -help」。
// 那个分支没有意义：flag.Usage 被调用时用户就是在找参数说明，多一次
// 往返只是麻烦。所以这里无条件打全。
func Usage(s string) {
	out := flag.CommandLine.Output()
	// 错误显式丢弃：这是往 usage 输出写字，写失败时也无处可报 ——
	// 报错本身就得往同一个已经坏掉的流里写。flag.PrintDefaults 出于
	// 同样的理由也不返回 error。
	_, _ = fmt.Fprintf(out, "%s\n", s)
	flag.PrintDefaults()
}
