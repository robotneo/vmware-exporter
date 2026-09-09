package esxcli

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ErrEmptyResponse 表示 ESXi 主机回了一个语法合法、但缺少 returnval 元素的
// SOAP 响应。
//
// 这不是理论情况：govmomi 的 soap.RoundTrip 只在传输层或 SOAP fault 层出错时
// 返回 error。响应体里没有 <ExecuteSoapResponse> 或没有 <returnval> 时它照常
// 返回 nil error，而 ExecuteSoapBody.Res / ExecuteSoapResponse.Returnval 都是
// 指针，于是调用方拿到 (nil, nil)。以前这里直接解引用，一台状态异常的 ESXi
// 就能让整个 exporter 进程 panic 退出 —— 影响的是所有 target，不只是那一台。
//
// 用 sentinel 而不是即席 fmt.Errorf，是为了让调用方能 errors.Is 区分「主机没
// 给数据」与「网络/认证失败」，前者通常意味着该主机的 esxcli 子系统不可用，
// 重试无益。
var ErrEmptyResponse = errors.New("esxcli: host returned a SOAP response without a returnval element")

// Run 在指定主机上执行一条 esxcli 命令并返回原始响应体。
//
// client 取 soap.RoundTripper 而非 *vim25.Client：底层的 ExecuteSoap /
// RetrieveManagedMethodExecuter 本来就只要求这个接口，收窄之后测试才能塞进
// 一个返回空响应的桩，否则这条路径只能靠真实 ESXi 才能覆盖 —— 而这个包此前
// 覆盖率正是 0%。*vim25.Client 实现了该接口，所有既有调用点无需改动。
func Run(ctx context.Context, client soap.RoundTripper, host types.ManagedObjectReference, command []string, arguments map[string]string) (interface{}, error) {

	// command 会被切成 command[:len(command)-1] 拼 moid，空切片时那个表达式
	// 是 command[:-1]，直接 panic。调用方传空切片属于编程错误，但让它以
	// 越界 panic 的形式出现在采集协程里，排查成本远高于一条明确的 error。
	if len(command) == 0 {
		return nil, errors.New("esxcli: command must not be empty")
	}

	mme, err := GetHostMME(ctx, client, &host)
	if err != nil {
		return nil, err
	}

	request := ExecuteSoapRequest{
		This:     *mme,
		Moid:     "ha-cli-handler-" + strings.Join(command[:len(command)-1], "-"),
		Method:   "vim.EsxCLI." + strings.Join(command, "."),
		Version:  "urn:vim25/5.0",
		Argument: ConfigArguments(arguments),
	}

	x, err := ExecuteSoap(ctx, client, &request)
	if err != nil {
		return nil, err
	}

	// 这三层判断缺一不可，且顺序固定：先 x（响应元素本身可能缺失），
	// 再 x.Returnval（元素在但 returnval 缺失），最后才是 fault。
	if x == nil || x.Returnval == nil {
		return nil, fmt.Errorf("%w (method %s)", ErrEmptyResponse, request.Method)
	}

	if x.Returnval.Fault != nil {
		return nil, errors.New(x.Returnval.Fault.FaultMsg)
	}

	return x.Returnval.Response, nil
}

func ConfigArguments(args map[string]string) []ReflectManagedMethodExecuterSoapArgument {

	var sargs []ReflectManagedMethodExecuterSoapArgument

	for argname, argvalue := range args {

		sargs = append(sargs, ReflectManagedMethodExecuterSoapArgument{Name: argname, Val: fmt.Sprintf("<%s>%s</%s>", argname, argvalue, argname)})
	}

	return sargs

}

// GetHostMME 取回主机的 ReflectManagedMethodExecuter 引用，后续每条 esxcli
// 命令都要用它作为 _this。
//
// 这是两个 esxcli collector 里每台主机的第一次调用，所以它是最先被异常主机
// 命中的解引用点。
func GetHostMME(ctx context.Context, client soap.RoundTripper, host *types.ManagedObjectReference) (*types.ManagedObjectReference, error) {

	req := RetrieveManagedMethodExecuterRequest{
		This: *host,
	}

	res, err := RetrieveManagedMethodExecuter(ctx, client, &req)
	if err != nil {
		return &types.ManagedObjectReference{}, err
	}

	if res == nil || res.Returnval == nil {
		return &types.ManagedObjectReference{},
			fmt.Errorf("%w (RetrieveManagedMethodExecuter on %s)", ErrEmptyResponse, host.Value)
	}

	return &res.Returnval.ManagedObjectReference, nil
}

// GetSOAP 执行一条已构造好的请求，并把响应体反序列化进 data。
func GetSOAP(ctx context.Context, client soap.RoundTripper, request *ExecuteSoapRequest, data interface{}) error {
	res, err := ExecuteSoap(ctx, client, request)
	if err != nil {
		return fmt.Errorf("error executing soap request: %w", err)
	}

	if res == nil || res.Returnval == nil {
		return fmt.Errorf("%w (method %s)", ErrEmptyResponse, request.Method)
	}

	if res.Returnval.Fault != nil {
		// 这里原先是 fmt.Errorf("error at xml return value: %s", err)，而 err
		// 在这个分支里必然是 nil（上面 err != nil 已经 return 了），于是每个
		// esxcli fault 都被报成 "error at xml return value: %!s(<nil>)" ——
		// 主机明确告诉了我们哪里错了，日志里却什么都看不到。
		return fmt.Errorf("esxcli fault on %s: %s", request.Method, res.Returnval.Fault.FaultMsg)
	}

	if err := xml.Unmarshal([]byte(res.Returnval.Response), data); err != nil {
		return fmt.Errorf("error unmarshalling xml: %w", err)
	}

	return nil
}
