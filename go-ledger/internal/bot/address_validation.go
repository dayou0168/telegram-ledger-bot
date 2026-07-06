package bot

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

func (b *Bot) handleAddressValidation(ctx context.Context, msg telegram.Message, user storage.User, address string, now time.Time) error {
	validation, err := b.store.RecordAddressValidation(ctx, msg.Chat.ID, address, user, now)
	if err != nil {
		return err
	}
	caption := formatAddressValidationCaption(validation)
	if validation.VerifyCount == 1 {
		imageBytes, err := buildAddressVerifyPNG(address)
		if err != nil {
			return err
		}
		_, err = b.tg.SendPhotoBytes(ctx, msg.Chat.ID, "usdt-address-verify.png", imageBytes, caption, map[string]any{
			"reply_to_message_id": msg.MessageID,
		})
		return err
	}
	_, err = b.tg.SendMessage(ctx, msg.Chat.ID, caption, map[string]any{
		"reply_to_message_id": msg.MessageID,
	})
	return err
}

func formatAddressValidationCaption(v storage.AddressValidation) string {
	var out strings.Builder
	out.WriteString("验证地址：")
	out.WriteString(v.Address)
	out.WriteString("\n验证次数：")
	out.WriteString(formatInt(v.VerifyCount))
	if v.PreviousUserName != "" {
		out.WriteString("\n上次发送人：")
		out.WriteString(v.PreviousUserName)
	}
	out.WriteString("\n本次发送人：")
	out.WriteString(v.LastUserName)
	if v.VerifyCount == 1 {
		out.WriteString("\n状态：首次发送")
	}
	return out.String()
}

func buildAddressVerifyPNG(address string) ([]byte, error) {
	const scale = 2
	small := image.NewRGBA(image.Rect(0, 0, 620, 205))
	draw.Draw(small, small.Bounds(), &image.Uniform{C: color.RGBA{R: 0x0a, G: 0xa8, B: 0x7f, A: 0xff}}, image.Point{}, draw.Src)
	fillRect(small, image.Rect(14, 118, 606, 174), color.RGBA{R: 0xdf, G: 0x78, B: 0x09, A: 0xff})
	fillRect(small, image.Rect(14, 118, 606, 119), color.RGBA{R: 0xff, G: 0xe5, B: 0x9d, A: 0xff})
	fillRect(small, image.Rect(14, 173, 606, 174), color.RGBA{R: 0xff, G: 0xe5, B: 0x9d, A: 0xff})

	drawText(small, 28, 34, "TRON", color.RGBA{R: 0x1b, G: 0x48, B: 0x3e, A: 0xff})
	drawText(small, 172, 72, "USDT ANTI-TAMPER CHECK", color.RGBA{R: 0xff, G: 0xf0, B: 0x63, A: 0xff})
	drawText(small, 172, 94, "VERIFY THE ADDRESS BEFORE PAYMENT", color.RGBA{R: 0x13, G: 0x46, B: 0x3a, A: 0xff})
	drawBoldText(small, 34, 151, address, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})

	large := image.NewRGBA(image.Rect(0, 0, small.Bounds().Dx()*scale, small.Bounds().Dy()*scale))
	for y := 0; y < large.Bounds().Dy(); y++ {
		for x := 0; x < large.Bounds().Dx(); x++ {
			large.Set(x, y, small.At(x/scale, y/scale))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, large); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fillRect(img draw.Image, rect image.Rectangle, c color.Color) {
	draw.Draw(img, rect, &image.Uniform{C: c}, image.Point{}, draw.Src)
}

func drawText(img draw.Image, x, y int, text string, c color.Color) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(text)
}

func drawBoldText(img draw.Image, x, y int, text string, c color.Color) {
	drawText(img, x, y, text, c)
	drawText(img, x+1, y, text, c)
	drawText(img, x, y+1, text, c)
}
