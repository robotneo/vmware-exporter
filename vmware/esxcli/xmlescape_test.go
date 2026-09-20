package esxcli

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestConfigArgumentsEscapesXML（M-04 核实项）
//
// ConfigArguments 把参数包成内层 XML 字符串放进 Val：
//
//	<val><nicname>值</nicname></val>
//
// 担心的注入面是：值（或参数名）里若带 < & >，能否闭合当前元素、注入额外
// XML 元素/实体，破坏 ESXi 端解析。结论应当是"不能"：Val 在结构体里是普通
// 内容字段 `xml:"val"`（不是 `,innerxml`/cdata），Go 的 encoding/xml 在最终
// 序列化时对整段内容做实体转义 —— 连那对 <nicname> 尖括号也一起转义，由
// ESXi 端再解一层（这是 esxcli over SOAP 的既定编码方式）。
//
// 本测试直接 marshal 该结构体，把这个"依赖 encoding 转义"的隐式前提钉成显式
// 断言；一旦有人把 Val 改成 innerxml 或手工拼原始信封，立刻变红。
func TestConfigArgumentsEscapesXML(t *testing.T) {
	// 经典 XML 注入探针：尝试闭合 val、插入伪装元素、再放实体起始符。
	evilValue := `a</val><injected xmlns="http://evil.example">pwned</injected><x>&`

	out, err := xml.Marshal(ConfigArguments(map[string]string{"nicname": evilValue})[0])
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)

	if strings.Contains(got, "<injected") || strings.Contains(got, "</val><injected") {
		t.Errorf("malicious value produced a live injected element (no escaping):\n%s", got)
	}
	if !strings.Contains(got, "&lt;injected") {
		t.Errorf("value's '<' was not entity-escaped:\n%s", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Errorf("value's '&' was not entity-escaped:\n%s", got)
	}

	// 往返一致性：ESXi 端对 val 做二次 XML 解码后必须拿回原始字符串，证明
	// 转义只做安全包裹、未改变语义。
	var decoded ReflectManagedMethodExecuterSoapArgument
	if err := xml.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}
	if !strings.Contains(decoded.Val, evilValue) {
		t.Errorf("round-trip changed the payload:\n val=%q\nwant substring %q", decoded.Val, evilValue)
	}
}

// TestConfigArgumentsEscapesArgName 覆盖参数名这一侧：ConfigArguments 既把名字
// 当 XML 元素名拼进 Val，又把它放在独立的 <name> 字段。元素名里的尖括号不
// 构成合法标签（会进入内层字符串），而 <name> 是结构体内容字段，编码层必须
// 转义。这里断言编码不 panic 且 <name> 字段里的特殊字符被实体化。
func TestConfigArgumentsEscapesArgName(t *testing.T) {
	args := ConfigArguments(map[string]string{"a&<b": "v"})
	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
	out, err := xml.Marshal(args[0])
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "a&amp;&lt;b") {
		t.Errorf("argument name special chars not escaped in <name>:\n%s", got)
	}
}
