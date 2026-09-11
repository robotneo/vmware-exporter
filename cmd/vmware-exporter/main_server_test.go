package main

import (
	"testing"
	"time"
)

func TestNewHTTPServerSetsTimeouts(t *testing.T) {
	srv := newHTTPServer()

	// ReadHeaderTimeout 是抗 Slowloris 的关键：0（零值）意味着无限期等待，
	// 一个只连上、慢慢发 header 的客户端就能永久占住一个 goroutine。
	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}

	// 反向护栏：一次抓取可能跑满 -vmware.timeout（默认 60s）。若有人给
	// server 加了 WriteTimeout，正常的慢响应会被掐断。这里把它钉成 0，
	// 改的人需要显式重新评估与抓取超时的关系。
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0; a write deadline would cut off scrapes that legitimately run up to -vmware.timeout", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0; a read deadline spanning the whole request would also cut off long scrapes", srv.ReadTimeout)
	}

	// 常量本身的合理性：Header 等待应在秒级、短于抓取超时量级，Idle 应有限。
	if srv.ReadHeaderTimeout <= 0 || srv.ReadHeaderTimeout > 10*time.Second {
		t.Errorf("ReadHeaderTimeout %v out of a sane Slowloris-defence range", srv.ReadHeaderTimeout)
	}
}
