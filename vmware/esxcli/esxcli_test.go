package esxcli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// stubRT 是一个 soap.RoundTripper 桩，用来复现 govmomi 在响应体缺元素时的
// 行为：RoundTrip 返回 nil error，但 res 里的指针字段一个都没被填。
//
// 这正是真实 ESXi 在 esxcli 子系统不可用时的表现，也是这个包此前每一处
// panic 的触发条件。用桩而不是 vcsim：vcsim 不模拟 ha-cli-handler，
// 而我们要测的恰好是「主机什么都不给」这种响应。
type stubRT struct {
	// fill 在 RoundTrip 里被调用，用来按需填充 res。为 nil 表示什么都不填
	// —— 也就是「空响应」这个我们最关心的场景。
	//
	// 同时接收 req 是必需的：govmomi 的 RoundTrip 收到的是**两个不同的
	// body 实例**（reqBody 与 resBody），Req 字段只在前者上。只接 res
	// 的话想断言请求内容就会解引用到 nil。
	fill func(req, res soap.HasFault)
	err  error
}

func (s *stubRT) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if s.err != nil {
		return s.err
	}
	if s.fill != nil {
		s.fill(req, res)
	}
	return nil
}

var testHost = types.ManagedObjectReference{Type: "HostSystem", Value: "host-42"}

// 反序列化目标的本地副本。真正的 NicListResponse 住在 collectors 包里，
// 从这里 import 会成环 —— 而这个测试关心的只是「响应体有没有被解进去」，
// 用一个形状相同的本地类型即可。
type nicListInfo struct {
	Name        string `xml:"Name"`
	Description string `xml:"Description"`
}

type nicListResponse struct {
	DataObject []nicListInfo `xml:"DataObject"`
}

// fillMME 让 RetrieveManagedMethodExecuter 返回一个正常的 MME 引用，
// 用于把测试推进到 ExecuteSoap 那一步。
func fillMME(_, res soap.HasFault) {
	if b, ok := res.(*RetrieveManagedMethodExecuterBody); ok {
		b.Res = &RetrieveManagedMethodExecuterResponse{
			Returnval: &ReflectManagedMethodExecuter{
				ManagedObjectReference: types.ManagedObjectReference{
					Type: "ReflectManagedMethodExecuter", Value: "ha-cli-handler",
				},
			},
		}
	}
}

// TestGetHostMMEEmptyResponseReturnsError 锁住第一个解引用点。
//
// 缺陷版是 `return &res.Returnval.ManagedObjectReference, nil` —— 注入它，
// 这个测试会以 panic 的形式失败（不是普通 FAIL），因为那正是生产里发生的事。
func TestGetHostMMEEmptyResponseReturnsError(t *testing.T) {
	_, err := GetHostMME(context.Background(), &stubRT{}, &testHost)
	if err == nil {
		t.Fatal("GetHostMME returned nil error for an empty SOAP response; " +
			"the old code dereferenced res.Returnval here and crashed the process")
	}
	if !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("error is not ErrEmptyResponse, callers cannot tell an empty "+
			"response from a transport failure: %v", err)
	}
}

// TestGetHostMMEPropagatesTransportError 是上一条的对照组。
//
// 少了它，一个「无论如何都返回 ErrEmptyResponse」的实现也能通过 —— 那会把
// 网络故障误报成「主机没数据」，运维照着排查方向完全错。
func TestGetHostMMEPropagatesTransportError(t *testing.T) {
	want := errors.New("dial tcp: connection refused")

	_, err := GetHostMME(context.Background(), &stubRT{err: want}, &testHost)
	if !errors.Is(err, want) {
		t.Fatalf("transport error was not propagated: got %v, want %v", err, want)
	}
	if errors.Is(err, ErrEmptyResponse) {
		t.Error("transport failure was reported as ErrEmptyResponse")
	}
}

// fillExecuteSoap 构造一个 ExecuteSoapResponse。returnval 为 nil 时只填
// Res，模拟「响应元素在、returnval 缺失」—— 与完全空响应是两个不同的
// 解引用点，必须分别覆盖。
func fillExecuteSoap(returnval *ReflectManagedMethodExecuterSoapResult) func(_, res soap.HasFault) {
	return func(_, res soap.HasFault) {
		if b, ok := res.(*ExecuteSoapBody); ok {
			b.Res = &ExecuteSoapResponse{Returnval: returnval}
		}
	}
}

func TestGetSOAPEmptyResponseReturnsError(t *testing.T) {
	req := &ExecuteSoapRequest{Method: "vim.EsxCLI.network.nic.list"}

	// 两个场景对应源码里两个不同的 nil 检查：res 本身为 nil（旧代码在
	// `if res.Returnval != nil` 这一行就崩），以及 res 在但 Returnval 为 nil
	// （旧代码活过了那个 if，倒在下面的 res.Returnval.Response）。
	cases := map[string]*stubRT{
		"response element missing": {},
		"returnval missing":        {fill: fillExecuteSoap(nil)},
	}

	for name, rt := range cases {
		var data nicListResponse

		err := GetSOAP(context.Background(), rt, req, &data)
		if err == nil {
			t.Errorf("%s: GetSOAP returned nil error; the old code panicked here", name)
			continue
		}
		if !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("%s: error is not ErrEmptyResponse: %v", name, err)
		}
	}
}

// TestGetSOAPFaultReportsFaultMessage 锁住那条被写坏的错误信息。
//
// 旧代码是 fmt.Errorf("error at xml return value: %s", err)，而 err 在这个
// 分支里必为 nil —— 主机明确说了哪里错，日志里却只有 %!s(<nil>)。断言必须
// 检查 FaultMsg 真的出现在错误文本里，只断言「err != nil」是抓不住的。
func TestGetSOAPFaultReportsFaultMessage(t *testing.T) {
	const faultMsg = "EsxCLI.network.nic.get: Unknown nic vmnic99"

	rt := &stubRT{fill: fillExecuteSoap(&ReflectManagedMethodExecuterSoapResult{
		Fault: &ReflectManagedMethodExecuterSoapFault{FaultMsg: faultMsg},
	})}

	var data struct{}
	err := GetSOAP(context.Background(), rt, &ExecuteSoapRequest{Method: "m"}, &data)

	if err == nil {
		t.Fatal("GetSOAP accepted a response carrying a fault")
	}
	if !strings.Contains(err.Error(), faultMsg) {
		t.Errorf("the host told us what went wrong but the message was dropped;\n got: %v\nwant it to contain: %s", err, faultMsg)
	}
	if strings.Contains(err.Error(), "<nil>") {
		t.Errorf("error text still formats a nil err: %v", err)
	}
}

// TestGetSOAPUnmarshalsResponse 是空响应/fault 两条测试的对照组：没有它，
// 一个「永远返回 error」的实现也能让上面全绿。
func TestGetSOAPUnmarshalsResponse(t *testing.T) {
	const body = `<root><DataObject><Name>vmnic0</Name><Description>Intel X710</Description></DataObject></root>`

	rt := &stubRT{fill: fillExecuteSoap(&ReflectManagedMethodExecuterSoapResult{Response: body})}

	var data nicListResponse
	if err := GetSOAP(context.Background(), rt, &ExecuteSoapRequest{Method: "m"}, &data); err != nil {
		t.Fatalf("GetSOAP failed on a well-formed response: %v", err)
	}

	if len(data.DataObject) != 1 || data.DataObject[0].Name != "vmnic0" {
		t.Fatalf("response was not unmarshalled into data: %+v", data)
	}
}

// TestRunEmptyCommandReturnsError 锁住 command[:len(command)-1] 的越界。
//
// 空切片时那个表达式是 command[:-1]，运行时 panic。传空切片是编程错误，
// 但让它表现为采集协程里的 slice bounds panic，排查成本远高于一条 error。
//
// 桩必须用 fillMME 让 GetHostMME 成功：那个切片表达式在 GetHostMME **之后**
// 的 request 构造里。用空桩的话 GetHostMME 先报 ErrEmptyResponse 就返回了，
// 测试会因为另一条原因变绿 —— 反向验证时正是这样漏过去的（去掉 guard 后
// 测试依然 PASS）。
func TestRunEmptyCommandReturnsError(t *testing.T) {
	_, err := Run(context.Background(), &stubRT{fill: fillMME}, testHost, nil, nil)
	if err == nil {
		t.Fatal("Run accepted an empty command; the old code panicked with a slice bounds error")
	}
	if errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("test is not exercising the slice bounds path: it failed at "+
			"GetHostMME instead (%v); the stub must let GetHostMME succeed", err)
	}
}

// TestRunEmptyExecuteSoapResponseReturnsError 覆盖 Run 里最隐蔽的一处。
//
// 旧代码在 L36 写了 `if x.Returnval != nil { ... }`，看起来做了 nil 检查，
// 但真正解引用的 `return x.Returnval.Response` 在那个 if 块**外面**。
// 这里让 MME 正常返回、只让 ExecuteSoap 给空响应，正好走到那一行。
func TestRunEmptyExecuteSoapResponseReturnsError(t *testing.T) {
	// 第一次 RoundTrip（RetrieveManagedMethodExecuter）正常填充，
	// 第二次（ExecuteSoap）什么都不填 —— fillMME 只认 MME 那个 body 类型，
	// 落到 ExecuteSoapBody 上时它是空操作，恰好实现了这个组合。
	rt := &stubRT{fill: fillMME}

	_, err := Run(context.Background(), rt, testHost, []string{"network", "nic", "list"}, nil)
	if err == nil {
		t.Fatal("Run returned nil error for an empty ExecuteSoap response; " +
			"the nil check at the top only guarded the fault branch, not the return")
	}
	if !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("error is not ErrEmptyResponse: %v", err)
	}
}

// TestRunReturnsResponse 是对照组，同时锁住 moid / method 的拼法 ——
// 没有它，一个「永远报 ErrEmptyResponse」的实现能让上面两条全绿。
func TestRunReturnsResponse(t *testing.T) {
	var gotMoid, gotMethod string

	rt := &stubRT{fill: func(req, res soap.HasFault) {
		fillMME(req, res)
		if b, ok := req.(*ExecuteSoapBody); ok {
			gotMoid = b.Req.Moid
			gotMethod = b.Req.Method
		}
		if b, ok := res.(*ExecuteSoapBody); ok {
			b.Res = &ExecuteSoapResponse{
				Returnval: &ReflectManagedMethodExecuterSoapResult{Response: "<root/>"},
			}
		}
	}}

	out, err := Run(context.Background(), rt, testHost, []string{"network", "nic", "list"}, nil)
	if err != nil {
		t.Fatalf("Run failed on a well-formed response: %v", err)
	}
	if out != "<root/>" {
		t.Errorf("got response %q, want %q", out, "<root/>")
	}

	// 最后一段是方法名、不进 moid：ha-cli-handler-network-nic + .list。
	if gotMoid != "ha-cli-handler-network-nic" {
		t.Errorf("moid = %q, want ha-cli-handler-network-nic", gotMoid)
	}
	if gotMethod != "vim.EsxCLI.network.nic.list" {
		t.Errorf("method = %q, want vim.EsxCLI.network.nic.list", gotMethod)
	}
}
