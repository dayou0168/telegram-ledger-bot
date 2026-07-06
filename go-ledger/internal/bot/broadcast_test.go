package bot

import "testing"

func TestFormatBroadcastProgressText(t *testing.T) {
	if got := formatBroadcastProgressText("chat", 1); got != "发送中..." {
		t.Fatalf("single chat progress = %q", got)
	}
	if got := formatBroadcastProgressText("group", 3); got != "广播发送中：目标 3 个。" {
		t.Fatalf("group progress = %q", got)
	}
}

func TestFormatBroadcastResultText(t *testing.T) {
	if got := formatBroadcastResultText("chat", "11", 1, 0, false); got != "11：发送完成。" {
		t.Fatalf("single chat success = %q", got)
	}
	if got := formatBroadcastResultText("chat", "测试群", 0, 1, true); got != "测试群：发送失败。\n通知所有人：开启" {
		t.Fatalf("single chat failed notify = %q", got)
	}
	if got := formatBroadcastResultText("group", "财务", 2, 1, false); got != "广播完成：成功 2 个，失败 1 个。" {
		t.Fatalf("group result = %q", got)
	}
}
