package telegram

import (
	"encoding/json"
	"testing"
)

func TestUpdateDecodesMemberDiscoveryFields(t *testing.T) {
	const payload = `{
		"update_id": 101,
		"chat_member": {
			"chat": {"id": -1001, "type": "supergroup", "title": "test"},
			"from": {"id": 1, "first_name": "admin"},
			"date": 1721200000,
			"old_chat_member": {"status": "left", "user": {"id": 2, "first_name": "target"}},
			"new_chat_member": {"status": "member", "user": {"id": 2, "username": "target_user", "first_name": "target"}}
		}
	}`
	var update Update
	if err := json.Unmarshal([]byte(payload), &update); err != nil {
		t.Fatal(err)
	}
	if update.ChatMember == nil || update.ChatMember.NewChatMember.User.Username != "target_user" {
		t.Fatalf("chat member not decoded: %+v", update.ChatMember)
	}

	const messagePayload = `{
		"update_id": 102,
		"message": {
			"message_id": 8,
			"date": 1721200001,
			"chat": {"id": -1001, "type": "supergroup"},
			"from": {"id": 1, "first_name": "admin"},
			"text": "target",
			"entities": [{"type": "text_mention", "offset": 0, "length": 6, "user": {"id": 2, "username": "target_user", "first_name": "target"}}],
			"new_chat_members": [{"id": 3, "username": "new_user", "first_name": "new"}],
			"reply_to_message": {"message_id": 7, "date": 1721199999, "chat": {"id": -1001, "type": "supergroup"}, "from": {"id": 4, "username": "reply_user", "first_name": "reply"}}
		}
	}`
	if err := json.Unmarshal([]byte(messagePayload), &update); err != nil {
		t.Fatal(err)
	}
	if update.Message == nil || update.Message.ReplyTo == nil || len(update.Message.NewChatMembers) != 1 || len(update.Message.Entities) != 1 {
		t.Fatalf("message discovery fields not decoded: %+v", update.Message)
	}
}
