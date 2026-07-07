package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func (b *Bot) handlePrivateShortcut(ctx context.Context, msg telegram.Message, user storage.User, text string) (bool, error) {
	switch strings.TrimSpace(text) {
	case "✍开始记账", "开始记账":
		return true, b.sendPrivateText(ctx, msg.Chat.ID, msg.MessageID, privateStartHelp())
	case "📃详细说明", "详细说明":
		return true, b.sendPrivateText(ctx, msg.Chat.ID, msg.MessageID, privateDetailedHelp())
	case "🔎查询UID", "查询UID":
		return true, b.sendUIDLookup(ctx, msg.Chat.ID, msg.MessageID, user.ID)
	case "⚙后台管理", "后台管理", "👥广播权限", "广播权限", "🔁广播替换", "广播替换":
		return true, b.sendAdminEntry(ctx, msg.Chat.ID, msg.MessageID)
	default:
		return false, nil
	}
}

func (b *Bot) sendUIDLookup(ctx context.Context, chatID, replyTo, userID int64) error {
	userIsBot := false
	keyboard := telegram.ReplyKeyboardMarkup{
		Keyboard: [][]telegram.KeyboardButton{
			{{
				Text: "选择用户获取UID",
				RequestUsers: &telegram.KeyboardButtonRequestUsers{
					RequestID:   1001,
					UserIsBot:   &userIsBot,
					MaxQuantity: 5,
				},
			}},
			{{Text: "菜单"}},
		},
		ResizeKeyboard:  true,
		OneTimeKeyboard: true,
	}
	text := fmt.Sprintf("你的 Telegram UID：%d\n\n可以点击下方按钮选择用户获取 UID，也可以让对方私聊机器人发送“我的ID”。", userID)
	return b.enqueueReliableText(ctx, sendPriorityNormal, "uid_lookup", messageScopedDedupe("uid_lookup", chatID, replyTo), chatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        keyboard,
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func (b *Bot) sendPrivateText(ctx context.Context, chatID, replyTo int64, text string) error {
	return b.enqueueReplyText(ctx, sendPriorityNormal, "private_text", chatID, replyTo, text, nil, time.Now().In(b.loc))
}

func (b *Bot) sendAdminEntry(ctx context.Context, chatID, replyTo int64) error {
	text := "后台管理用于设置广播分组、广播权限、广播替换和查看已保存群。"
	opts := map[string]any{"reply_to_message_id": replyTo}
	if b.cfg.PublicBillBaseURL != "" {
		link := b.cfg.PublicBillBaseURL + "/admin"
		text += "\n\n后台入口：" + link
		opts["reply_markup"] = telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{{Text: "打开后台管理", URL: link}},
		}}
	} else {
		text += "\n\n当前没有配置 PUBLIC_BILL_BASE_URL，公网使用时请先在 Compose 里填写你的 HTTPS 域名。"
	}
	return b.enqueueReliableText(ctx, sendPriorityNormal, "admin_entry", messageScopedDedupe("admin_entry", chatID, replyTo), chatID, text, opts, reliableMessageRef{}, time.Now().In(b.loc))
}

func privateStartHelp() string {
	return strings.TrimSpace(`群内使用：
开始：激活/恢复记账
停止：暂停记账
+100：入款人民币
+100/7.1：按指定汇率入款
+100U：入款 U
下发100U：下发 U
+0 / 账单：查看当前账单
上课：开启全员发言权限
下课：关闭全员发言权限

机器人被邀请进群或群里有人发言后，会自动保存群。`)
}

func privateDetailedHelp() string {
	return strings.TrimSpace(`常用记账命令：
+100 备注
+100/7.1 备注
+100U 备注
下发100U 备注
设置费率3
设置汇率7.1
设置日切04
关闭日切
撤销 / 撤销入款 / 撤销下发
清除今日账单 / 清除全部账单
上课 / 下课
通知所有人

查询：
Z0：显示 OKX OTC 商家所有实时汇率 TOP 10
Z1 -0.1：按第 1 档下浮 0.1 设置汇率
查询T...：查询 TRON 地址余额和最近 USDT 流水

广播和后台：
私聊点击“群发广播”可选择全部授权群、分组或单群；点击“分组广播”可直接选择分组。选定目标后，直接发送文字、图片或文件即可连续广播。
选择广播目标后可切换“通知所有人”，广播投递后会在目标群 @ 已发言成员。
“后台管理”用于查看群列表、设置分组、权限和单群发送回复替换。`)
}
