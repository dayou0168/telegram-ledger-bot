package bot

import (
	"bytes"
	"context"
	_ "embed"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	addressVerifyWidth  = 1232
	addressVerifyHeight = 397
)

var (
	addressVerifyTemplateOnce sync.Once
	addressVerifyTemplateImg  *image.RGBA
	addressVerifyTemplateErr  error
)

//go:embed assets/address_verify_template.png
var addressVerifyTemplatePNG []byte

func (b *Bot) handleAddressValidation(ctx context.Context, msg telegram.Message, user storage.User, address string, now time.Time) error {
	validation, err := b.store.RecordAddressValidation(ctx, msg.Chat.ID, address, user, now)
	if err != nil {
		return err
	}
	b.hydrateAddressValidationNames(ctx, msg.Chat.ID, &validation)
	if validation.VerifyCount == 1 {
		var account *tron.Account
		if info, err := b.tron.FetchAccount(ctx, address, b.cfg.USDTContract); err == nil {
			account = &info
		}
		caption := formatFirstAddressValidationCaption(validation, account, b.loc)
		imageBytes, err := buildAddressVerifyPNG(address)
		if err != nil {
			_, sendErr := b.tg.SendMessage(ctx, msg.Chat.ID, caption, map[string]any{
				"reply_to_message_id": msg.MessageID,
				"parse_mode":          "HTML",
			})
			return sendErr
		}
		_, err = b.tg.SendPhotoBytes(ctx, msg.Chat.ID, "usdt-address-verify.png", imageBytes, caption, map[string]any{
			"reply_to_message_id": msg.MessageID,
			"parse_mode":          "HTML",
		})
		return err
	}
	caption := formatRepeatAddressValidationCaption(validation)
	_, err = b.tg.SendMessage(ctx, msg.Chat.ID, caption, map[string]any{
		"reply_to_message_id": msg.MessageID,
		"parse_mode":          "HTML",
	})
	return err
}

func (b *Bot) hydrateAddressValidationNames(ctx context.Context, chatID int64, v *storage.AddressValidation) {
	v.FirstUserName = b.addressValidationUserLabel(ctx, chatID, v.FirstUserID, v.FirstUserName)
	v.PreviousUserName = b.addressValidationUserLabel(ctx, chatID, v.PreviousUserID, v.PreviousUserName)
	v.LastUserName = b.addressValidationUserLabel(ctx, chatID, v.LastUserID, v.LastUserName)
}

func (b *Bot) addressValidationUserLabel(ctx context.Context, chatID, userID int64, fallback string) string {
	if userID == 0 {
		return fallback
	}
	user, ok, err := b.store.GetUser(ctx, chatID, userID)
	if err == nil && ok {
		if user.Username != "" {
			return "@" + user.Username
		}
		if user.DisplayName != "" {
			return user.DisplayName
		}
	}
	return fallback
}

func formatFirstAddressValidationCaption(v storage.AddressValidation, account *tron.Account, loc *time.Location) string {
	var out strings.Builder
	out.WriteString("💎 <code>")
	out.WriteString(html.EscapeString(v.Address))
	out.WriteString("</code>")
	if account != nil {
		created := "暂无"
		if account.CreatedAt > 0 {
			created = formatMilliTime(account.CreatedAt, loc)
		}
		out.WriteString("\n🌐 创建： ")
		out.WriteString(html.EscapeString(created))
		out.WriteString("\n├ ▣ USDT： ")
		out.WriteString(formatTokenAmount(account.USDTBalance, firstPositive(account.USDTDecimals, 6), 2))
		out.WriteString("\n├ ▣ TRX： ")
		out.WriteString(formatTokenAmount(account.BalanceSun, 6, 6))
	} else {
		out.WriteString("\n🌐 创建： 暂无")
		out.WriteString("\n├ ▣ USDT： 暂无")
		out.WriteString("\n├ ▣ TRX： 暂无")
	}
	out.WriteString("\n└✅ 状态： 首次验证")
	out.WriteString("\n验证次数： ")
	out.WriteString(formatInt(v.VerifyCount))
	appendSenderLines(&out, v)
	return out.String()
}

func formatRepeatAddressValidationCaption(v storage.AddressValidation) string {
	var out strings.Builder
	out.WriteString("验证地址： <code>")
	out.WriteString(html.EscapeString(v.Address))
	out.WriteString("</code>")
	out.WriteString("\n验证次数： ")
	out.WriteString(formatInt(v.VerifyCount))
	appendSenderLines(&out, v)
	return out.String()
}

func appendSenderLines(out *strings.Builder, v storage.AddressValidation) {
	if v.PreviousUserName != "" {
		out.WriteString("\n上次发送人： ")
		out.WriteString(html.EscapeString(v.PreviousUserName))
	}
	out.WriteString("\n本次发送人： ")
	out.WriteString(html.EscapeString(v.LastUserName))
}

func buildAddressVerifyPNG(address string) ([]byte, error) {
	template, err := addressVerifyTemplate()
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(template.Bounds())
	draw.Draw(img, img.Bounds(), template, image.Point{}, draw.Src)
	addressFace := fitAddressFont(address, 1136, 57, 38)
	box := image.Rect(28, 232, addressVerifyWidth-28, 342)
	drawCenteredTextInRect(img, box, address, addressFace, color.White)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addressVerifyTemplate() (*image.RGBA, error) {
	addressVerifyTemplateOnce.Do(func() {
		addressVerifyTemplateImg, addressVerifyTemplateErr = buildAddressVerifyTemplate()
	})
	return addressVerifyTemplateImg, addressVerifyTemplateErr
}

func buildAddressVerifyTemplate() (*image.RGBA, error) {
	if len(addressVerifyTemplatePNG) > 0 {
		decoded, err := png.Decode(bytes.NewReader(addressVerifyTemplatePNG))
		if err == nil {
			rgba := image.NewRGBA(decoded.Bounds())
			draw.Draw(rgba, rgba.Bounds(), decoded, image.Point{}, draw.Src)
			return rgba, nil
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, addressVerifyWidth, addressVerifyHeight))
	draw.Draw(img, img.Bounds(), image.NewUniform(hexColor(0x03, 0xa7, 0x7b)), image.Point{}, draw.Src)

	logoFace := loadFontFace(latinRegularFonts(), 34)
	titleFace := loadFontFace(cjkBoldFonts(), 54)
	subtitleFace := loadFontFace(cjkBoldFonts(), 28)

	drawTronMark(img, 32, 48)
	drawTextTop(img, 82, 53, "TRON", logoFace, hexColor(0x1c, 0x33, 0x34))
	drawCenteredTextTop(img, 82, "USDT防篡改验证核对", titleFace, hexColor(0xff, 0xf0, 0x5a))
	drawCenteredTextTop(img, 166, "《请双方谨慎核对地址是否与图中一致,如有误停止付款》", subtitleFace, hexColor(0x17, 0x33, 0x33))

	box := image.Rect(28, 232, addressVerifyWidth-28, 342)
	fillRect(img, box, hexColor(0xdb, 0x70, 0x00))
	drawRectOutline(img, box, color.White)
	return img, nil
}

func drawTronMark(img draw.Image, x, y int) {
	red := hexColor(0xf0, 0x18, 0x3a)
	outer := []image.Point{{X: x, Y: y}, {X: x + 42, Y: y + 12}, {X: x + 14, Y: y + 48}}
	fillPolygon(img, outer, red)
	inner := []image.Point{{X: x + 9, Y: y + 8}, {X: x + 34, Y: y + 15}, {X: x + 15, Y: y + 37}}
	fillPolygon(img, inner, color.White)
	drawLine(img, x+9, y+8, x+15, y+37, red, 2)
	drawLine(img, x+15, y+37, x+34, y+15, red, 2)
	drawLine(img, x+34, y+15, x+9, y+8, red, 2)
}

func fillPolygon(img draw.Image, points []image.Point, c color.Color) {
	if len(points) < 3 {
		return
	}
	minY, maxY := points[0].Y, points[0].Y
	for _, p := range points[1:] {
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}
	}
	for y := minY; y <= maxY; y++ {
		var xs []int
		for i, p1 := range points {
			p2 := points[(i+1)%len(points)]
			if (p1.Y <= y && p2.Y > y) || (p2.Y <= y && p1.Y > y) {
				x := p1.X + (y-p1.Y)*(p2.X-p1.X)/(p2.Y-p1.Y)
				xs = append(xs, x)
			}
		}
		if len(xs) < 2 {
			continue
		}
		if xs[0] > xs[1] {
			xs[0], xs[1] = xs[1], xs[0]
		}
		for x := xs[0]; x <= xs[1]; x++ {
			img.Set(x, y, c)
		}
	}
}

func drawLine(img draw.Image, x0, y0, x1, y1 int, c color.Color, width int) {
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		for ox := -width / 2; ox <= width/2; ox++ {
			for oy := -width / 2; oy <= width/2; oy++ {
				img.Set(x0+ox, y0+oy, c)
			}
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func fillRect(img draw.Image, rect image.Rectangle, c color.Color) {
	draw.Draw(img, rect, image.NewUniform(c), image.Point{}, draw.Src)
}

func drawRectOutline(img draw.Image, rect image.Rectangle, c color.Color) {
	fillRect(img, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+1), c)
	fillRect(img, image.Rect(rect.Min.X, rect.Max.Y-1, rect.Max.X, rect.Max.Y), c)
	fillRect(img, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+1, rect.Max.Y), c)
	fillRect(img, image.Rect(rect.Max.X-1, rect.Min.Y, rect.Max.X, rect.Max.Y), c)
}

func drawCenteredTextTop(img draw.Image, top int, text string, face font.Face, c color.Color) {
	x := (addressVerifyWidth - measureText(face, text)) / 2
	drawTextTop(img, x, top, text, face, c)
}

func drawCenteredTextInRect(img draw.Image, rect image.Rectangle, text string, face font.Face, c color.Color) {
	x := rect.Min.X + (rect.Dx()-measureText(face, text))/2
	metrics := face.Metrics()
	height := (metrics.Ascent + metrics.Descent).Ceil()
	baseline := rect.Min.Y + (rect.Dy()-height)/2 + metrics.Ascent.Ceil() - 4
	drawTextBaseline(img, x, baseline, text, face, c)
}

func drawTextTop(img draw.Image, x, top int, text string, face font.Face, c color.Color) {
	drawTextBaseline(img, x, top+face.Metrics().Ascent.Ceil(), text, face, c)
}

func drawTextBaseline(img draw.Image, x, baseline int, text string, face font.Face, c color.Color) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(text)
}

func fitAddressFont(text string, maxWidth, startSize, minSize int) font.Face {
	for size := startSize; size >= minSize; size-- {
		face := loadFontFace(latinBoldFonts(), float64(size))
		if measureText(face, text) <= maxWidth {
			return face
		}
	}
	return loadFontFace(latinBoldFonts(), float64(minSize))
}

func measureText(face font.Face, text string) int {
	d := &font.Drawer{Face: face}
	return d.MeasureString(text).Ceil()
}

func loadFontFace(paths []string, size float64) font.Face {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var parsedFont *opentype.Font
		if strings.HasSuffix(strings.ToLower(path), ".ttc") {
			collection, err := opentype.ParseCollection(raw)
			if err != nil {
				continue
			}
			parsedFont, err = collection.Font(0)
			if err != nil {
				continue
			}
		} else {
			parsedFont, err = opentype.Parse(raw)
			if err != nil {
				continue
			}
		}
		face, err := opentype.NewFace(parsedFont, &opentype.FaceOptions{
			Size:    size,
			DPI:     72,
			Hinting: font.HintingFull,
		})
		if err == nil {
			return face
		}
	}
	return basicfont.Face7x13
}

func cjkBoldFonts() []string {
	return []string{
		"C:/Windows/Fonts/msyhbd.ttc",
		"C:/Windows/Fonts/simhei.ttf",
		"/usr/share/fonts/noto-cjk/NotoSansCJK-Bold.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	}
}

func latinRegularFonts() []string {
	return []string{
		"C:/Windows/Fonts/arial.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",
	}
}

func latinBoldFonts() []string {
	return []string{
		"C:/Windows/Fonts/arialbd.ttf",
		"C:/Windows/Fonts/impact.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSansCondensed-Bold.ttf",
		"/usr/share/fonts/dejavu/DejaVuSansCondensed-Bold.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	}
}

func hexColor(r, g, b uint8) color.RGBA {
	return color.RGBA{R: r, G: g, B: b, A: 0xff}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
