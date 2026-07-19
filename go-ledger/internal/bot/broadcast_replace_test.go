package bot

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestBroadcastReplacementPlanFullMatrix(t *testing.T) {
	media := []struct {
		name string
		msg  telegram.Message
	}{
		{name: "text", msg: telegram.Message{Text: "original text"}},
		{name: "photo", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "original-photo"}}}},
		{name: "photo caption", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "original-photo"}}, Caption: "original caption"}},
	}
	for _, medium := range media {
		for _, fixedImage := range []bool{false, true} {
			for _, fixedText := range []bool{false, true} {
				for _, mode := range []string{"chat", "group", "all"} {
					for _, enabled := range []bool{false, true} {
						name := fmt.Sprintf("%s/image=%t/text=%t/mode=%s/enabled=%t", medium.name, fixedImage, fixedText, mode, enabled)
						t.Run(name, func(t *testing.T) {
							setting := storage.BroadcastReplaceSetting{Enabled: enabled}
							if fixedImage {
								setting.ImageData = []byte("fixed-image")
							}
							if fixedText {
								setting.Text = " fixed text "
							}
							got := broadcastReplacementPlan(mode, false, medium.msg, setting)
							want := expectedBroadcastReplacement(medium.name, fixedImage, fixedText, mode, enabled)
							if got != want {
								t.Fatalf("plan=%+v want=%+v", got, want)
							}
						})
					}
				}
			}
		}
	}
	if got := broadcastReplacementPlan("chat", true, media[2].msg, storage.BroadcastReplaceSetting{Enabled: true, Text: "fixed", ImageData: []byte("image")}); got.Kind != "" {
		t.Fatalf("already replaced delivery plan=%+v", got)
	}
}

func expectedBroadcastReplacement(media string, fixedImage, fixedText bool, mode string, enabled bool) broadcastReplacement {
	if mode != "chat" || !enabled {
		return broadcastReplacement{}
	}
	switch media {
	case "text":
		if fixedText {
			return broadcastReplacement{Kind: "text", Text: "fixed text"}
		}
	case "photo":
		if fixedImage {
			return broadcastReplacement{Kind: "photo"}
		}
	case "photo caption":
		if fixedImage {
			caption := "original caption"
			if fixedText {
				caption = "fixed text"
			}
			return broadcastReplacement{Kind: "photo", Caption: caption}
		}
		if fixedText {
			return broadcastReplacement{Kind: "caption", Caption: "fixed text"}
		}
	}
	return broadcastReplacement{}
}

func TestBroadcastReplacementNeverChangesUpstreamOriginal(t *testing.T) {
	dispatch := broadcastDispatchContext{SenderLabel: "@operator", TargetDisplay: "单群 · 目标群"}
	text := telegram.Message{Text: "original text"}
	setting := storage.BroadcastReplaceSetting{Enabled: true, ImageData: []byte("fixed-image")}
	if plan := broadcastReplacementPlan("chat", false, text, setting); plan.Kind != "" {
		t.Fatalf("pure text must not become a fixed image: %+v", plan)
	}
	payload, companion, ok := broadcastUpstreamPayloads(text, dispatch)
	if !ok || companion != nil || payload.Type != "text" || !strings.Contains(payload.Text, "original text") {
		t.Fatalf("upstream text=%+v companion=%+v ok=%t", payload, companion, ok)
	}

	photo := telegram.Message{Photo: []telegram.Photo{{FileID: "original-photo"}}, Caption: "original caption"}
	setting.Text = "fixed caption"
	if plan := broadcastReplacementPlan("chat", false, photo, setting); plan.Kind != "photo" || plan.Caption != "fixed caption" {
		t.Fatalf("single-chat target plan=%+v", plan)
	}
	payload, companion, ok = broadcastUpstreamPayloads(photo, dispatch)
	if !ok || companion != nil || payload.FileID != "original-photo" || !strings.Contains(payload.Caption, "original caption") || strings.Contains(payload.Caption, "fixed caption") {
		t.Fatalf("upstream photo=%+v companion=%+v ok=%t", payload, companion, ok)
	}
}
