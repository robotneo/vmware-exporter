package main

import (
	"net/http"
	"net/http/pprof"
)

// registerDiagnostics 按需挂载 Go 运行时的 profiling 端点（net/http/pprof）。
//
// 致命陷阱：传入的 mux 绝不能是 http.DefaultServeMux。
//
//	import net/http/pprof 会在包的 init() 里把 /debug/pprof/ 系列以 Go 1.22+
//	的「带方法」模式（如 "GET /debug/pprof/cmdline"）注册进 DefaultServeMux。
//	本函数又按 flag 在同一 mux 上注册「不带方法」的同名路径。两种模式一个
//	路径更具体、一个方法更少，ServeMux 判定为冲突，在启动期直接 panic
//	（"conflicts with pattern …"），进程无法启动。main 因此改用一个全新的
//	ServeMux（pprof init 碰不到它），端点仍只由 -web.enable-pprof 精确控制。
//
// 为什么默认不挂载、要单独开一个 flag：
//
//	profiling 端点是强力的排障工具，也是一个不该对不可信网络敞开的面。
//	/debug/pprof/profile 与 /debug/pprof/trace 会让进程在采样窗口内承担
//	额外开销，/debug/pprof/goroutine?debug=2 会打印全部 goroutine 栈，
//	未鉴权暴露等于免费送人一份内部结构与运行状态。因此与 /debug 调试页
//	不同，这个端点即便在开发心智下也保持「默认关闭」：出问题时显式打开、
//	抓完即关，而不是长期挂在端口上。
//
// 路径刻意选在 /debug/pprof/：这是 net/http/pprof 的既定前缀，也是 Go
// 工具链与 pprof 工具默认查找的位置，`go tool pprof http://host:port/
// debug/pprof/profile` 可直接工作。ui.go 的 registerUI 只精确注册了
// /debug，没有占用 /debug/ 子树，因此两者不冲突。
func registerDiagnostics(mux *http.ServeMux, enable bool) {
	if !enable {
		return
	}

	// net/http/pprof 没有提供一次性挂载全部处理器的辅助函数，它在自己的
	// init() 里把处理器注册到 http.DefaultServeMux（故必须传入非该 mux）。
	// 这里用 Handler/HandlerFunc 显式逐个挂载，与 net/http/pprof 的 init
	// 完全同源，使端点受 flag 控制而非 import 副作用。
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// 上面的 Index 对 /debug/pprof/<name> 形态会按名字查找具体 profile；
	// goroutine/heap/threadcreate/block/mutex 这些命名 profile 经由 Index
	// 内部的 handler(name) 提供，不需要逐条注册。
}
