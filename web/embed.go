// Package web 提供 exporter 的 HTTP 界面：概览页与调试页。
//
// 为什么单独成包而不是继续内嵌在 vmware-exporter.go 里：改动前落地页是一段
// 拼接在 main() 里的字符串常量，既没有 <!DOCTYPE> 也没有 <html>/<body> 的
// 开标签（只有闭标签），浏览器靠容错解析才显示得出来。把 HTML、CSS 与 JS
// 拆成真实文件之后，编辑器能做语法高亮与格式化，而 go:embed 保证它们仍然
// 编进同一个二进制 —— 部署方式完全不变，不需要额外分发静态目录。
//
// 嵌入而不是运行时读文件是刻意的：exporter 常以 scratch 容器镜像运行，镜像
// 里除了二进制什么都没有；从磁盘读模板会在容器里直接失败。
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
)

// assets 是页面用到的全部静态资源。
//
// 注意 embed 的路径是包目录的相对路径，且不受 .gitignore 影响但受
// .dockerignore 影响 —— 已确认 .dockerignore 未排除 web/，镜像构建拿得到。
//
//go:embed index.html debug.html app.css app.js
var assets embed.FS

// Collector 描述一个采集器在页面上的呈现方式。
//
// 这些字段全部来自 vmware/collectors 的注册表，不是页面自己维护的第二份
// 清单：Cost 那一栏的文字对应注册表注释里说明的开销与前置条件（逐主机串行
// 的 esxcli、需要开启 vSAN 与性能服务的 vsan 系列）。运维在勾选之前就该
// 知道哪些选项会显著拉长一次抓取。
type Collector struct {
	Name           string
	Description    string
	DefaultEnabled bool
	// Cost 为空表示没有特别的开销提示。
	Cost string
}

// Data 是两个页面共用的模板数据。
type Data struct {
	// ExporterName 与 Version 用于顶栏。
	ExporterName string
	Version      string

	// Collectors 驱动概览页的清单与调试页的复选框。
	Collectors []Collector

	// DebugConsole 为 false 时概览页不显示 /debug 入口。
	DebugConsole bool

	// MetricsTargetDisabled 反映 -disable.exporter.target：为 true 时
	// /metrics 只输出 exporter 自身的指标，不去连 vCenter。页面必须说明
	// 这一点，否则「/metrics 里为什么没有 vmware_* 指标」会变成一个
	// 需要翻源码才能回答的问题。
	MetricsTargetDisabled bool
}

var tmpl = template.Must(template.ParseFS(assets, "index.html", "debug.html"))

// Asset 返回一个嵌入的静态文件。
func Asset(name string) ([]byte, error) {
	return assets.ReadFile(name)
}

// RenderIndex 渲染概览页。
func RenderIndex(d Data) ([]byte, error) {
	return render("index.html", d)
}

// RenderDebug 渲染调试页。
func RenderDebug(d Data) ([]byte, error) {
	return render("debug.html", d)
}

// render 把模板渲染进内存再整体返回，而不是直接写 http.ResponseWriter。
//
// 这样模板出错时还没有任何字节写出去，调用方可以干净地返回 500；边写边渲染
// 的话，错误会追加在一份已经发出的半截 HTML 后面，浏览器显示出来的是一个
// 看起来正常、实际缺了下半部分的页面。
func render(name string, d Data) ([]byte, error) {
	var buf bytes.Buffer

	if err := tmpl.ExecuteTemplate(&buf, name, d); err != nil {
		return nil, fmt.Errorf("could not render %s: %w", name, err)
	}

	return buf.Bytes(), nil
}
