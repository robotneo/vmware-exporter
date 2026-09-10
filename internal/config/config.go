// Package config 接管命令行、配置文件与环境变量三来源的 flag 解析。
//
// 这部分逻辑此前来自 prezhdarov/prometheus-exporter/pkg/config。随框架一并
// 移除时不能顺手删掉：-file 与 -envflag.* 是仓库对外承诺的接口，
// packaging/systemd/config.yaml 用 -file、docker-compose.yml 用 -envflag.*，
// 两份 README 也都在讲它们，而 docker-compose 里正是靠 -envflag.enable 把密码从
// command: 里挪出去，避免同主机上任何能读 /proc 的进程看到明文。删掉它等于把一个
// 已经修好的泄露口重新打开。
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
	"sync"

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

// 重载状态。三者都在 Parse 里一次性填好，之后只被 Reload 读。
//
// baseline 是「命令行 + 各 flag 默认值」这一层，也就是 applyFileAndEnv 之前
// 的 flag 快照。Reload 必须先回到这一层再叠加文件与环境变量，否则删掉配置
// 文件里的一行不会有任何效果：那个 flag 会一直留着上一次重载时写进去的值，
// 而运维看到的配置文件里已经没有它了。
//
// cliSet 记录哪些 flag 是命令行显式给的。Reload 跳过它们，这是「命令行 >
// 文件 > 环境变量」在重载路径上的实现 —— 优先级顺序在启动与重载之间必须
// 一致，否则 systemctl reload 会静默改掉 ExecStart 上写死的参数。
var (
	baseline    map[string]string
	cliSet      map[string]bool
	parseCalled bool
)

// mu 串行化「重载写 flag」与「抓取读 flag」。
//
// 为什么必须有它：Reload 通过 flag.FlagSet.Set 改值，而 flag 包的 setter
// 是**裸写**，没有任何同步原语 —— 标准库 flag.go 里 intValue.Set 的最后一行
// 就是 `*i = intValue(v)`。与此同时，本 exporter 的配置刻意设计成在请求
// 路径上解引用（这正是「改 flag 值就能热重载」成立的原因），于是 SIGHUP
// 协程写、HTTP 抓取协程读，构成教科书式的数据竞争。
//
// 竞争的后果不是「读到旧值」那么温和 —— Go 内存模型对无同步的并发读写不作
// 任何保证，撕裂读在理论上是允许的，而实践中更常见的是编译器把循环里的
// 解引用提到循环外，让新值永远不生效。
//
// 选 RWMutex 而不是把每个 flag 换成 atomic：flag 的类型由标准库定下，
// 换不了；而读侧本来就是「每轮抓取快照一次」的粗粒度，RLock 的开销可以忽略。
//
// 用法契约：
//   - 写侧只有 Reload 的提交阶段，它自己持写锁，调用方无需关心。
//   - 读侧调用 RLock/RUnlock，或直接用 Snapshot 辅助函数。**必须一次性
//     读完本轮需要的全部 flag**，分多次 RLock 会读到重载前后混合的配置。
var mu sync.RWMutex

// RLock/RUnlock 供读侧在快照 flag 值时使用。
//
// 导出这两个而不是「给每个 flag 配一个 getter」，是因为 flag 变量分散在
// 各个包里（vmware/api 有 8 个、根包有若干），getter 方案要求每加一个
// flag 就记得同步加一个 getter —— 漏了不会有任何编译错误或测试失败，
// 只会让那个 flag 悄悄回到无保护状态。
func RLock()   { mu.RLock() }
func RUnlock() { mu.RUnlock() }

// Snapshot 在读锁保护下执行 fn，fn 里应当把需要的 flag 值拷进局部变量。
//
// 比裸用 RLock/RUnlock 安全的地方在于它保证配对，且把「一次性读完」这个
// 要求变成了代码结构上的约束而不是注释里的叮嘱。
func Snapshot(fn func()) {
	mu.RLock()
	defer mu.RUnlock()

	fn()
}

// reloadExempt 列出重载时不可生效的 flag。
//
// 这三个都在进程启动时被一次性消费掉，之后改动它们只会让 flag 的值与进程的
// 实际行为不一致：
//
//   - http.address     web.ListenAndServe 已经 bind 了这个地址
//   - web.config.file  TLS 证书与 basic auth 在监听时装载
//   - log.format       promslog.New 按 format 选定 handler 类型，
//     且 promslog.Format 自己的注释写明 "Not concurrency-safe"
//
// -log.level 不在此列：promslog.Level 内部是 slog.LevelVar，并发安全且可变，
// 所以日志级别是可以热改的 —— 这恰好是重载最常见的用途（临时开 debug 排查
// 问题，不想中断抓取）。
var reloadExempt = map[string]bool{
	"http.address":    true,
	"web.config.file": true,
	"log.format":      true,
}

// Parse 解析 flag，优先级由高到低：命令行 > 配置文件 > 环境变量。
//
// 这个顺序不是任选的：命令行是运维当场的显式意图，配置文件是这台机器的
// 固化配置，环境变量是容器编排注入的兜底。反过来的话，临时用命令行覆盖
// 一个参数去排查问题就做不到了。
func Parse() error {
	flag.Parse()

	// 快照必须在 applyFileAndEnv 之前取：它要记的是「命令行与默认值」这一层，
	// 而 applyFileAndEnv 会把文件与环境变量的值写进同一批 flag。顺序颠倒的话
	// baseline 会把文件里的值也当成默认值，Reload 就再也回不到干净的起点。
	baseline = snapshot(flag.CommandLine)
	cliSet = explicitlySet(flag.CommandLine)
	parseCalled = true

	return applyFileAndEnv(flag.CommandLine, *file, *enable)
}

// Reload 重新读取 -file 与环境变量，把结果写回同一批 flag 指针。
//
// 为什么这样就够了：这个 exporter 几乎所有配置都在**请求路径上**才被解引用。
// 每次抓取都会重新读 -collector.* 开关（collector.Registered）、
// -collector.max-concurrency、-disable.exporter.target，每次登录都会重新读
// -vmware.username/password/vcenter/schema/insecureTLS/timeout/interval/
// granularity。所以改掉 flag 的值，下一轮抓取自动生效 —— 不需要重建 handler，
// 也不需要重启监听。
//
// 失败时保持旧配置不变，这是本函数最重要的性质：坏配置不应该让一个正在正常
// 工作的 exporter 降级成半套配置。实现方式见 reload —— 先在一个临时 FlagSet
// 上把新配置算完整，只有全部成功才提交到真实 flag。
//
// 三个 reloadExempt 里的 flag 不会被改动，调用方应把返回的 skipped 记进日志，
// 让运维知道「这几项改了也没用，得 restart」而不是以为已经生效。
func Reload() (skipped []string, err error) {
	if !parseCalled {
		return nil, fmt.Errorf("Reload called before Parse")
	}

	return reload(flag.CommandLine, *file, *enable, baseline, cliSet)
}

func snapshot(fs *flag.FlagSet) map[string]string {
	out := make(map[string]string)
	fs.VisitAll(func(f *flag.Flag) {
		out[f.Name] = f.Value.String()
	})

	return out
}

// explicitlySet 返回在命令行上被显式赋值过的 flag 名集合。
//
// fs 必须已经 Parse 过：Visit 只遍历被设置过的 flag，在 Parse 之前它返回空集。
func explicitlySet(fs *flag.FlagSet) map[string]bool {
	out := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		out[f.Name] = true
	})

	return out
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

// reload 是 Reload 的可测内核。拆出来的理由与 applyFileAndEnv 相同：
// 测试不能安全地驱动全局 flag.CommandLine。
//
// 原子性的实现分两段。计算段在一个**影子 FlagSet** 上重放整套配置：影子集
// 与 fs 同名但值独立，所以文件不存在、YAML 语法错误、未知 flag 名这些失败
// 碰不到真实配置。提交段把算好的值逐个写进 fs，任何一个失败就把这一批全部
// 恢复原值。
//
// 提交段必须回滚而不能只是「停下」，理由见提交循环里的注释：标准库的
// intValue.Set 在解析失败时已经把 0 写进去了才返回 error。
func reload(fs *flag.FlagSet, path string, envEnabled bool,
	base map[string]string, cli map[string]bool) (skipped []string, err error) {

	shadow, err := shadowOf(fs, base, cli)
	if err != nil {
		return nil, err
	}

	// 在影子集上叠加文件与环境变量。这里失败就直接返回，真实 flag 一个字节
	// 都没动过。
	if err := applyFileAndEnv(shadow, path, envEnabled); err != nil {
		return nil, err
	}

	// 计划阶段：算出「哪些 flag 要改成什么」，先不动真实 flag。
	type change struct {
		name string
		from string
		to   string
	}

	var plan []change

	// 从这里开始持写锁，直到提交（或回滚）结束。
	//
	// 锁必须覆盖计划阶段而不只是提交循环：计划阶段用 target.Value.String()
	// 读真实 flag 的当前值，那些值同时是回滚要用的原始值。不在锁内读的话，
	// 一次并发抓取正在读同一批 flag，读写照样撞上；更糟的是 from 可能记到
	// 一个中间态，回滚会把配置写成一份从未存在过的组合。
	//
	// shadowOf 与 applyFileAndEnv 在锁外是有意的：它们只碰影子 FlagSet，
	// 而读文件与解析环境变量可能耗时（文件在网络盘上时尤甚），把它们圈进
	// 写锁会让每轮抓取在重载期间白等。
	mu.Lock()
	defer mu.Unlock()

	shadow.VisitAll(func(f *flag.Flag) {
		target := fs.Lookup(f.Name)
		if target == nil {
			return
		}

		newValue := f.Value.String()
		oldValue := target.Value.String()

		if oldValue == newValue {
			return
		}

		// reloadExempt 的 flag 允许在配置里写，但不生效。收集起来交给调用方
		// 记日志：静默忽略会让运维改完 -http.address 后以为端口换了。
		if reloadExempt[f.Name] {
			skipped = append(skipped, f.Name)
			return
		}

		plan = append(plan, change{name: f.Name, from: oldValue, to: newValue})
	})

	// 提交阶段。影子集是纯字符串的，不做类型校验，所以类型错误要到这里才暴露
	// （-vmware.interval=abc 在影子集上是合法字符串）。
	//
	// !!! 失败必须回滚，不能只是停下 !!!
	// flag.intValue.Set 的实现是 `v, err := ParseInt(...); *i = intValue(v);
	// return err` —— 解析失败时它**已经把 0 写进去了**才返回 error。也就是说
	// 一个坏值不仅自己没生效，还会把原来好的值清成 0。float64Value、
	// int64Value、uintValue、durationValue 全是同一个写法。
	//
	// 回滚用的是提交前从真实 flag 读出的字符串。那些值刚刚还在生效，
	// 必然能被自己的 Value.Set 接受。
	for i, c := range plan {
		if err := fs.Set(c.name, c.to); err == nil {
			continue
		} else {
			// 把已经改过的（含当前这个被写坏的）逐个恢复。
			for _, done := range plan[:i+1] {
				if rerr := fs.Set(done.name, done.from); rerr != nil {
					// 走到这里说明「刚才还在生效的值现在设不回去」，
					// 属于 flag.Value 实现有状态。没有更好的兜底，
					// 把两个错误一起报出来供人工介入。
					return nil, fmt.Errorf("cannot apply reloaded flag %s=%q (%w) and cannot restore %s to %q: %w",
						c.name, c.to, err, done.name, done.from, rerr)
				}
			}

			return nil, fmt.Errorf("cannot apply reloaded flag %s=%q, configuration left unchanged: %w", c.name, c.to, err)
		}
	}

	sort.Strings(skipped)

	return skipped, nil
}

// shadowOf 构造一个与 fs 形状相同、但值来自 base 的独立 FlagSet。
//
// 每个 flag 用 fs 里同名 flag 的 Value 类型无法直接克隆（flag.Value 是接口，
// 没有 Clone），所以这里走字符串：新建一个 stringValue 承载 base 的值。这对
// applyFileAndEnv 来说足够 —— 它只调用 Lookup 与 Set，不关心底层类型。
//
// 代价是影子集不做类型校验（-vmware.interval=abc 在影子集上能 Set 成功）。
// 这不是漏洞：提交阶段会把值 Set 进真实的 *int flag，那里会拒绝并返回
// commitErr。类型错误因此仍然报出来，只是报在提交阶段而非计算阶段。
//
// cli 里的 flag 被标记为「已设置」并跳过后续来源，这就是方案 A 的优先级实现。
func shadowOf(fs *flag.FlagSet, base map[string]string, cli map[string]bool) (*flag.FlagSet, error) {
	shadow := flag.NewFlagSet("reload", flag.ContinueOnError)
	shadow.SetOutput(new(strings.Builder))

	var names []string

	fs.VisitAll(func(f *flag.Flag) {
		value, ok := base[f.Name]
		if !ok {
			// 启动后新出现的 flag。正常情况不会发生（flag 都在 init 里注册），
			// 但如果发生了，用当前值兜底比丢掉这个 flag 安全。
			value = f.Value.String()
		}

		shadow.String(f.Name, value, f.Usage)

		if cli[f.Name] {
			names = append(names, f.Name)
		}
	})

	// 用 Parse 把命令行来源的 flag 标记成「已显式设置」，这样 applyFileAndEnv
	// 的 shadow.Visit 能看到它们并跳过。手工调 shadow.Set 做不到这一点 ——
	// Set 不会让 Visit 认为该 flag 被设置过（只有 Parse 走的路径会记录），
	// 而 applyFileAndEnv 的命令行优先级完全依赖 Visit。
	args := make([]string, 0, len(names))
	for _, name := range names {
		args = append(args, fmt.Sprintf("-%s=%s", name, base[name]))
	}

	if err := shadow.Parse(args); err != nil {
		return nil, fmt.Errorf("cannot rebuild the command line layer for reload: %w", err)
	}

	return shadow, nil
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
