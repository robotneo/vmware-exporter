package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prezhdarov/vmware-exporter/internal/config"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"

	"github.com/prometheus/common/promslog"
)

// handleReloadSignals 监听 SIGHUP 并重新加载配置，直到 ctx 被取消。
//
// 为什么需要它：在此之前进程没有任何信号处理器，SIGHUP 走 Go 的默认处置 ——
// **终止进程**。也就是说 `systemctl reload` 会静默杀掉 exporter（unit 里
// 一度写着 ExecReload=/bin/kill -HUP $MAINPID，实测把服务打挂），运维改完
// 配置只能 restart，抓取因此出现一个缺口。
//
// 为什么重载只需要改 flag 的值：这个 exporter 的配置几乎全部在请求路径上
// 解引用。每次抓取重新读各 -collector.* 开关与 -collector.max-concurrency，
// 每次登录重新读 -vmware.* 那一组。所以 config.Reload 写回 flag 指针之后，
// **下一轮抓取自然用上新配置** —— 不需要重建 handler，不需要重启监听，
// 正在进行中的抓取也不受影响（它们已经拿到了自己那一份值）。
//
// promslogLevel 单独传进来是因为 -log.level 走的不是 flag 指针那条路：logger
// 在启动时已经构造好，热改级别要写它内部的 slog.LevelVar。promslog.Level
// 正是 LevelVar 的包装，并发安全。
func handleReloadSignals(ctx context.Context, logger *slog.Logger, promslogLevel *promslog.Level) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			reloadConfig(logger, promslogLevel)
		}
	}
}

// reloadConfig 执行一次重载并更新自监控指标。
//
// 失败时保持旧配置不变（由 config.Reload 保证原子性），只把指标打成 0 并记
// 一条 error。一个正在正常抓取的 exporter 不该因为配置文件里打错一个字符就
// 降级 —— 但也不能让失败无声无息，那样运维会以为改动已经生效。
func reloadConfig(logger *slog.Logger, promslogLevel *promslog.Level) {
	logger.Info("received SIGHUP, reloading configuration")

	skipped, err := config.Reload()
	if err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reload failed, keeping the previous configuration", "error", err)

		return
	}

	// -log.level 的值此时已经被 Reload 写回 flag，但 logger 内部的 LevelVar
	// 还是旧的，要显式同步过去。放在 ValidateFlags 之前：级别本身非法会被
	// Set 拒绝，那属于配置错误，应该和其他重载失败一样处理。
	//
	// 走快照而不是裸读 *logLevel：Reload 的写锁在返回时就释放了，这里已经
	// 在锁外。目前只有一个信号协程调 reloadConfig，但 Reload 是导出函数、
	// 没有任何东西保证这一点 —— 两次并发重载时这个读会撞上另一次的写。
	var level string

	config.Snapshot(func() { level = *logLevel })

	if err := promslogLevel.Set(level); err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reload failed, keeping the previous configuration", "error", err)

		return
	}

	// 重载后的 vmware.* 组合仍然要过一遍启动时的那套校验。不校验的话
	// -vmware.granularity=0 会在下一轮抓取时引发除零 —— 启动路径专门有
	// fail-fast 拦这个，重载路径漏掉就等于给它开了个后门。
	//
	// 注意这里已经无法回滚了：ValidateFlags 读的是 flag 的当前值，而 Reload
	// 已经提交。所以非法组合会带着错误日志留在进程里。这是有意的取舍 ——
	// 让 Reload 去理解 vmware 包的跨 flag 约束会把两个包耦在一起，而这种
	// 组合错误在 restart 时同样会被 fail-fast 拦住，不会悄悄长期存在。
	if err := vmware.ValidateFlags(); err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reloaded but the vmware.* combination is invalid; scrapes may fail until this is corrected", "error", err)

		return
	}

	if len(skipped) > 0 {
		logger.Warn("some settings changed but cannot be applied without a restart",
			"flags", strings.Join(skipped, ","))
	}

	configLastReloadSuccess.Set(1)
	configLastReloadTime.SetToCurrentTime()

	logger.Info("configuration reload succeeded", "log_level", level)
}
