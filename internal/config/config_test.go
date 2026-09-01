package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFlagSet 构造一个与生产 flag 集形状相似的独立 FlagSet。
//
// 不复用 flag.CommandLine：那是测试进程自己的 flag 集（-test.v 等都在里面），
// 往里注入会污染测试框架本身，而且测试之间会互相影响。
func newFlagSet(t *testing.T, args ...string) (*flag.FlagSet, map[string]*string) {
	t.Helper()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(new(strings.Builder))

	vals := map[string]*string{
		"vmware.vcenter":  fs.String("vmware.vcenter", "default-vc", "target"),
		"vmware.username": fs.String("vmware.username", "default-user", "user"),
		"vmware.password": fs.String("vmware.password", "default-pass", "password"),
	}

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v failed: %s", args, err)
	}

	return fs, vals
}

// writeConfig 落一个临时配置文件，返回路径。t.TempDir 会在测试结束时清理。
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vmware.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config file failed: %s", err)
	}

	return path
}

// setPrefix 临时改 -envflag.prefix 并在测试结束后还原。
//
// prefix 是包级 flag 指针，测试之间会互相影响，所以必须还原 —— 这与
// vmware/api 包的 restoreVMwareFlags 是同一个模式。
func setPrefix(t *testing.T, v string) {
	t.Helper()

	old := *prefix
	*prefix = v
	t.Cleanup(func() { *prefix = old })
}

// TestPrecedenceCommandLineBeatsFileAndEnv 锁住三来源的优先级顺序。
//
// 这个顺序不是任选的：命令行是运维当场的显式意图，配置文件是这台机器的
// 固化配置，环境变量是容器编排注入的兜底。反过来的话，临时用命令行覆盖
// 一个参数去排查问题就做不到了 —— 而那正是出故障时第一个要做的动作。
//
// 三个 flag 同时从三个不同来源赋值，是为了让一次断言覆盖全部两两关系：
//   - vcenter 三个来源都有 → 必须取命令行
//   - username 文件与环境变量都有 → 必须取文件
//   - password 只有环境变量 → 取环境变量
//
// 若只测「命令行 > 文件」，一个把环境变量放在最高优先级的实现也能通过。
func TestPrecedenceCommandLineBeatsFileAndEnv(t *testing.T) {
	setPrefix(t, "VMWARE_")

	path := writeConfig(t, "vmware.vcenter: from-file\nvmware.username: from-file\n")

	t.Setenv("VMWARE_vmware_vcenter", "from-env")
	t.Setenv("VMWARE_vmware_username", "from-env")
	t.Setenv("VMWARE_vmware_password", "from-env")

	fs, vals := newFlagSet(t, "-vmware.vcenter=from-cli")

	if err := applyFileAndEnv(fs, path, true); err != nil {
		t.Fatalf("applyFileAndEnv failed: %s", err)
	}

	cases := map[string]string{
		"vmware.vcenter":  "from-cli",
		"vmware.username": "from-file",
		"vmware.password": "from-env",
	}

	for name, want := range cases {
		if got := *vals[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestEnvIgnoredWhenDisabled 断言不开 -envflag.enable 时环境变量不生效。
//
// 这不是「多一道开关」而已：环境变量是隐式的输入来源，一个没打算用它的
// 部署里若同名变量恰好存在（比如镜像基础层带的），配置会被无声改写。
// 默认关闭意味着这种改写不会发生。
func TestEnvIgnoredWhenDisabled(t *testing.T) {
	setPrefix(t, "VMWARE_")
	t.Setenv("VMWARE_vmware_vcenter", "from-env")

	fs, vals := newFlagSet(t)

	if err := applyFileAndEnv(fs, "", false); err != nil {
		t.Fatalf("applyFileAndEnv failed: %s", err)
	}

	if got := *vals["vmware.vcenter"]; got != "default-vc" {
		t.Errorf("vmware.vcenter = %q, want the default %q (env must be ignored when disabled)", got, "default-vc")
	}
}

// TestUnknownFlagInFileIsRejected 断言配置文件里的未知 flag 名会报错。
//
// 框架的实现静默忽略它 —— 于是「我明明配了超时」与「超时没生效」之间
// 毫无线索：启动没有任何警告，指标看起来正常，只是那个参数根本没被读。
// 最常见的触发场景是升级后配置里残留了旧 flag 名。
func TestUnknownFlagInFileIsRejected(t *testing.T) {
	path := writeConfig(t, "vmware.vcenter: ok\nvmware.typoed.flag: oops\n")

	fs, _ := newFlagSet(t)

	err := applyFileAndEnv(fs, path, false)
	if err == nil {
		t.Fatal("applyFileAndEnv accepted an unknown flag name, want an error")
	}

	// 断言错误里带上了坏 flag 的名字：只说「配置文件有问题」的错误信息
	// 会让运维在一个几十行的配置里逐行找。
	if !strings.Contains(err.Error(), "vmware.typoed.flag") {
		t.Errorf("error %q does not name the offending flag", err)
	}
}

// TestMissingConfigFileIsRejected 断言 -file 指向不存在的路径时报错。
//
// 静默忽略是危险的：-file 是运维明确给出的指令，路径打错却当作「没有配置
// 文件」处理，会让 exporter 带着一整套默认值起来 —— 连的是默认 vCenter、
// 用的是默认凭证，而运维以为自己的配置生效了。
func TestMissingConfigFileIsRejected(t *testing.T) {
	fs, _ := newFlagSet(t)

	err := applyFileAndEnv(fs, filepath.Join(t.TempDir(), "does-not-exist.conf"), false)
	if err == nil {
		t.Fatal("applyFileAndEnv accepted a missing config file, want an error")
	}
}

// TestMalformedConfigFileIsRejected 断言 YAML 语法错误会报错而非被忽略。
func TestMalformedConfigFileIsRejected(t *testing.T) {
	path := writeConfig(t, "vmware.vcenter: [unclosed\n")

	fs, _ := newFlagSet(t)

	if err := applyFileAndEnv(fs, path, false); err == nil {
		t.Fatal("applyFileAndEnv accepted malformed YAML, want an error")
	}
}

// TestEnvFlagNamePreservesCase 锁住环境变量名的映射规则。
//
// 这条规则反直觉，所以必须有测试钉住：flag 名里的点换成下划线，但**大小写
// 原样保留**。-vmware.password 对应 VMWARE_vmware_password，不是
// VMWARE_VMWARE_PASSWORD。
//
// 拼错不会报错，只是那个环境变量被无声忽略 —— 在 docker-compose 里这意味着
// 密码没传进去，exporter 拿默认值去登录然后认证失败，而故障现场看起来像是
// vCenter 侧的问题。两份 README 与 check_config.py 都各自写明过这条规则，
// 这个测试是它在代码侧的锚点。
func TestEnvFlagNamePreservesCase(t *testing.T) {
	setPrefix(t, "VMWARE_")

	cases := map[string]string{
		"vmware.password":           "VMWARE_vmware_password",
		"collector.max-concurrency": "VMWARE_collector_max-concurrency",
		"log.level":                 "VMWARE_log_level",
	}

	for in, want := range cases {
		if got := EnvFlagName(in); got != want {
			t.Errorf("EnvFlagName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEnvFlagNameWithoutPrefix 断言空 prefix 时不加前缀。
func TestEnvFlagNameWithoutPrefix(t *testing.T) {
	setPrefix(t, "")

	if got := EnvFlagName("vmware.password"); got != "vmware_password" {
		t.Errorf("EnvFlagName = %q, want %q", got, "vmware_password")
	}
}

// TestSetLoggerRejectsInvalidValues 断言非法的 -log.* 值会报错。
//
// 框架把 promslog 的 Set 返回值丢掉了，于是 -log.level=verbose 静默降级成
// 默认等级。日志级别配错本身不致命，但排查问题时你会盯着一个「为什么没有
// debug 日志」的假象浪费时间 —— 而那时你正在排查别的故障。
func TestSetLoggerRejectsInvalidValues(t *testing.T) {
	valid := "logfmt"
	debug := "debug"

	if _, err := SetLogger(&valid, &debug); err != nil {
		t.Fatalf("SetLogger rejected a valid combination: %s", err)
	}

	badLevel := "verbose"
	if _, err := SetLogger(&valid, &badLevel); err == nil {
		t.Error("SetLogger accepted -log.level=verbose, want an error")
	}

	badFormat := "xml"
	if _, err := SetLogger(&badFormat, &debug); err == nil {
		t.Error("SetLogger accepted -log.format=xml, want an error")
	}
}
