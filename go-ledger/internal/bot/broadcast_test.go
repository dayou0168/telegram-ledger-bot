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
	if got := formatBroadcastResultText("chat", 1, 0, false); got != "发送完成。" {
		t.Fatalf("single chat success = %q", got)
	}
	if got := formatBroadcastResultText("chat", 0, 1, true); got != "发送失败。\n通知所有人：开启" {
		t.Fatalf("single chat failed notify = %q", got)
	}
	if got := formatBroadcastResultText("group", 2, 1, false); got != "广播完成：成功 2 个，失败 1 个。" {
		t.Fatalf("group result = %q", got)
	}
}

func TestBroadcastSessionControls(t *testing.T) {
	if !isBroadcastNotifyToggleText("通知所有人：关") || !isBroadcastNotifyToggleText("通知所有人:开") {
		t.Fatal("notify toggle text was not recognized")
	}
	if isBroadcastNotifyToggleText("通知所有人：今天发货") {
		t.Fatal("ordinary content should not be treated as notify toggle")
	}
	if !isBroadcastEndText("结束广播") || !isBroadcastEndText("取消广播") {
		t.Fatal("end broadcast text was not recognized")
	}
	if !isBroadcastSwitchTargetText("切换群") || !isBroadcastSwitchTargetText("切换目标") {
		t.Fatal("switch target text was not recognized")
	}
	if !isBroadcastMenuText("切换群") {
		t.Fatal("switch target should open broadcast target menu outside a session")
	}

	keyboard := broadcastSessionKeyboard("11", true)
	if len(keyboard.Keyboard) != 2 || len(keyboard.Keyboard[0]) != 1 || len(keyboard.Keyboard[1]) != 3 {
		t.Fatalf("unexpected keyboard shape: %#v", keyboard.Keyboard)
	}
	if keyboard.Keyboard[0][0].Text != "当前目标：11" {
		t.Fatalf("unexpected target label: %#v", keyboard.Keyboard[0])
	}
	if keyboard.Keyboard[1][0].Text != "通知所有人：开" || keyboard.Keyboard[1][1].Text != "切换群" || keyboard.Keyboard[1][2].Text != "结束广播" {
		t.Fatalf("unexpected keyboard labels: %#v", keyboard.Keyboard[1])
	}
}
