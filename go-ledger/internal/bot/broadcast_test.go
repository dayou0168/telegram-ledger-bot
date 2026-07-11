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
	if !keyboard.IsPersistent {
		t.Fatal("broadcast session keyboard should be persistent")
	}
	if keyboard.Keyboard[0][0].Text != "当前目标：11" {
		t.Fatalf("unexpected target label: %#v", keyboard.Keyboard[0])
	}
	if keyboard.Keyboard[1][0].Text != "通知所有人：开" || keyboard.Keyboard[1][1].Text != "切换群" || keyboard.Keyboard[1][2].Text != "结束广播" {
		t.Fatalf("unexpected keyboard labels: %#v", keyboard.Keyboard[1])
	}
}

func TestQuickReplyControlsAndExitState(t *testing.T) {
	keyboard := quickReplyKeyboard("出款群", true)
	if len(keyboard.Keyboard) != 2 || len(keyboard.Keyboard[1]) != 3 {
		t.Fatalf("unexpected quick reply keyboard shape: %#v", keyboard.Keyboard)
	}
	if keyboard.Keyboard[0][0].Text != "当前快速回复：出款群" {
		t.Fatalf("unexpected quick reply target label: %#v", keyboard.Keyboard[0])
	}
	if keyboard.Keyboard[1][0].Text != "结束快速回复" || keyboard.Keyboard[1][1].Text != "返回广播" || keyboard.Keyboard[1][2].Text != "取消" {
		t.Fatalf("unexpected quick reply exit controls: %#v", keyboard.Keyboard[1])
	}
	if !keyboard.IsPersistent {
		t.Fatal("quick reply keyboard should be persistent")
	}
	if !isQuickReplyEndText("结束快速回复") || !isQuickReplyEndText("返回广播") || !isQuickReplyEndText("取消") {
		t.Fatal("quick reply exit text was not recognized")
	}
	if !isQuickReplyStatusText("当前快速回复：出款群") {
		t.Fatal("quick reply status label was not recognized")
	}

	restored, ok := quickReplyReturnState(privateState{
		Mode:                   "quick_reply",
		ReturnMode:             "broadcast",
		ReturnTargetName:       "出款",
		ReturnChatIDs:          []int64{-1001, -1002},
		ReturnNotifyAll:        true,
		ReturnControlMessageID: 42,
	})
	if !ok {
		t.Fatal("quick reply should restore the previous broadcast state")
	}
	if restored.Mode == "quick_reply" || restored.Mode != "broadcast" {
		t.Fatalf("quick reply exit should leave quick_reply mode, got %q", restored.Mode)
	}
	if restored.TargetName != "出款" || len(restored.ChatIDs) != 2 || !restored.NotifyAll || restored.ControlMessageID != 42 {
		t.Fatalf("unexpected restored state: %+v", restored)
	}
	restored.ChatIDs[0] = 999
	if restored.ReturnChatIDs != nil {
		t.Fatalf("restored broadcast state should not keep quick reply return fields: %+v", restored)
	}

	if _, ok := quickReplyReturnState(privateState{Mode: "quick_reply"}); ok {
		t.Fatal("quick reply without return state should fall back to menu")
	}
}
