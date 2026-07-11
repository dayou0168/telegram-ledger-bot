package bot

import "testing"

func TestWithoutReplyOptionsStripsReplyFields(t *testing.T) {
	opts := map[string]any{
		"reply_to_message_id": 99,
		"reply_parameters":    map[string]any{"message_id": 99},
		"parse_mode":          "HTML",
	}
	cleaned := withoutReplyOptions(opts)
	if _, ok := cleaned["reply_to_message_id"]; ok {
		t.Fatal("reply_to_message_id should be stripped from ledger sends")
	}
	if _, ok := cleaned["reply_parameters"]; ok {
		t.Fatal("reply_parameters should be stripped from ledger sends")
	}
	if cleaned["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode should be preserved, got %#v", cleaned["parse_mode"])
	}
	if _, ok := opts["reply_to_message_id"]; !ok {
		t.Fatal("withoutReplyOptions should not mutate original opts")
	}
}

func TestReliableTextOutboxItemKeepsReplyForTraceMessages(t *testing.T) {
	item, err := reliableTextOutboxItem(sendPriorityNormal, "ledger_error", "ledger_error:-100:99", -100, "错误", map[string]any{
		"reply_to_message_id": 99,
	}, reliableMessageRef{})
	if err != nil {
		t.Fatalf("reliableTextOutboxItem() error = %v", err)
	}
	if item.ReplyToMessageID != 99 {
		t.Fatalf("ReplyToMessageID = %d, want 99", item.ReplyToMessageID)
	}
}
