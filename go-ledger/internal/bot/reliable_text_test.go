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
