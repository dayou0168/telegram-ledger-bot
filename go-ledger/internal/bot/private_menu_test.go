package bot

import "testing"

func TestPrivateMenuKeyboardUsesThreeRows(t *testing.T) {
	keyboard := privateMenuKeyboard()
	if len(keyboard.Keyboard) != 3 {
		t.Fatalf("private menu rows = %d, want 3", len(keyboard.Keyboard))
	}
	wants := [][]string{
		{"✍开始记账", "📃详细说明"},
		{"📡群发广播", "🔔地址监听"},
		{"🔎查询UID", "⚙后台管理"},
	}
	for i, row := range wants {
		if len(keyboard.Keyboard[i]) != len(row) {
			t.Fatalf("row %d width = %d, want %d", i, len(keyboard.Keyboard[i]), len(row))
		}
		for j, want := range row {
			if got := keyboard.Keyboard[i][j].Text; got != want {
				t.Fatalf("button[%d][%d] = %q, want %q", i, j, got, want)
			}
		}
	}
}
