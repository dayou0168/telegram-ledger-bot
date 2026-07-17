package bot

import "testing"

func TestBroadcastReplacementCaption(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		original   string
		want       string
	}{
		{
			name:       "configured text overrides original caption",
			configured: "  固定替换文字  ",
			original:   "原图片文字",
			want:       "固定替换文字",
		},
		{
			name:     "empty configured text preserves original caption",
			original: "原图片文字",
			want:     "原图片文字",
		},
		{
			name:       "whitespace configured text preserves original caption",
			configured: " \t\n ",
			original:   "原图片文字\n第二行",
			want:       "原图片文字\n第二行",
		},
		{
			name: "both empty remains empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := broadcastReplacementCaption(tt.configured, tt.original); got != tt.want {
				t.Fatalf("broadcastReplacementCaption() = %q, want %q", got, tt.want)
			}
		})
	}
}
