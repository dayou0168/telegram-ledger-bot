package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	values := url.Values{}
	if offset > 0 {
		values.Set("offset", strconv.FormatInt(offset, 10))
	}
	values.Set("timeout", strconv.Itoa(int(timeout.Seconds())))
	values.Set("allowed_updates", `["message","callback_query","my_chat_member"]`)
	var result []Update
	err := c.call(ctx, http.MethodGet, "getUpdates", values, nil, &result)
	return result, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, opts map[string]any) (Message, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	for k, v := range opts {
		payload[k] = v
	}
	var msg Message
	err := c.call(ctx, http.MethodPost, "sendMessage", nil, payload, &msg)
	return msg, err
}

func (c *Client) SendPhotoBytes(ctx context.Context, chatID int64, filename string, data []byte, caption string, opts map[string]any) (Message, error) {
	fields := map[string]string{
		"chat_id": strconv.FormatInt(chatID, 10),
	}
	if caption != "" {
		fields["caption"] = caption
	}
	for key, value := range opts {
		switch v := value.(type) {
		case string:
			fields[key] = v
		case int:
			fields[key] = strconv.Itoa(v)
		case int64:
			fields[key] = strconv.FormatInt(v, 10)
		case bool:
			fields[key] = strconv.FormatBool(v)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				return Message{}, err
			}
			fields[key] = string(raw)
		}
	}
	var msg Message
	err := c.callMultipart(ctx, "sendPhoto", fields, "photo", filename, data, &msg)
	return msg, err
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	return c.call(ctx, http.MethodPost, "answerCallbackQuery", nil, payload, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	return c.call(ctx, http.MethodPost, "deleteMessage", nil, payload, nil)
}

func (c *Client) LeaveChat(ctx context.Context, chatID int64) error {
	return c.call(ctx, http.MethodPost, "leaveChat", nil, map[string]any{"chat_id": chatID}, nil)
}

func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	payload := map[string]any{"chat_id": chatID, "user_id": userID}
	var member ChatMember
	err := c.call(ctx, http.MethodPost, "getChatMember", nil, payload, &member)
	return member, err
}

func (c *Client) SetChatPermissions(ctx context.Context, chatID int64, permissions ChatPermissions) error {
	payload := map[string]any{
		"chat_id":     chatID,
		"permissions": permissions,
	}
	return c.call(ctx, http.MethodPost, "setChatPermissions", nil, payload, nil)
}

func (c *Client) CopyMessage(ctx context.Context, chatID, fromChatID, messageID int64, opts map[string]any) (Message, error) {
	payload := map[string]any{
		"chat_id":      chatID,
		"from_chat_id": fromChatID,
		"message_id":   messageID,
	}
	for k, v := range opts {
		payload[k] = v
	}
	var msg Message
	err := c.call(ctx, http.MethodPost, "copyMessage", nil, payload, &msg)
	return msg, err
}

func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string, opts map[string]any) (Message, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	for k, v := range opts {
		payload[k] = v
	}
	var msg Message
	err := c.call(ctx, http.MethodPost, "editMessageText", nil, payload, &msg)
	return msg, err
}

func (c *Client) EditMessageCaption(ctx context.Context, chatID, messageID int64, caption string, opts map[string]any) (Message, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"caption":    caption,
	}
	for k, v := range opts {
		payload[k] = v
	}
	var msg Message
	err := c.call(ctx, http.MethodPost, "editMessageCaption", nil, payload, &msg)
	return msg, err
}

func (c *Client) EditMessagePhotoBytes(ctx context.Context, chatID, messageID int64, filename string, data []byte, caption string, opts map[string]any) (Message, error) {
	media := map[string]any{
		"type":  "photo",
		"media": "attach://photo",
	}
	if caption != "" {
		media["caption"] = caption
	}
	mediaRaw, err := json.Marshal(media)
	if err != nil {
		return Message{}, err
	}
	fields := map[string]string{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"message_id": strconv.FormatInt(messageID, 10),
		"media":      string(mediaRaw),
	}
	for key, value := range opts {
		switch v := value.(type) {
		case string:
			fields[key] = v
		case int:
			fields[key] = strconv.Itoa(v)
		case int64:
			fields[key] = strconv.FormatInt(v, 10)
		case bool:
			fields[key] = strconv.FormatBool(v)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				return Message{}, err
			}
			fields[key] = string(raw)
		}
	}
	var msg Message
	err = c.callMultipart(ctx, "editMessageMedia", fields, "photo", filename, data, &msg)
	return msg, err
}

func (c *Client) callMultipart(ctx context.Context, endpoint string, fields map[string]string, fileField, filename string, fileData []byte, out any) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile(fileField, filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(fileData); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	apiURL := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var wrapper struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return fmt.Errorf("telegram non-json response %s: %w", resp.Status, err)
	}
	if !wrapper.OK {
		return fmt.Errorf("telegram %s: %d %s", endpoint, wrapper.ErrorCode, wrapper.Description)
	}
	if out != nil {
		if err := json.Unmarshal(wrapper.Result, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) call(ctx context.Context, method, endpoint string, query url.Values, payload any, out any) error {
	apiURL := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, endpoint)
	if query != nil && len(query) > 0 {
		apiURL += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var wrapper struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return fmt.Errorf("telegram non-json response %s: %w", resp.Status, err)
	}
	if !wrapper.OK {
		return fmt.Errorf("telegram %s: %d %s", endpoint, wrapper.ErrorCode, wrapper.Description)
	}
	if out != nil {
		if err := json.Unmarshal(wrapper.Result, out); err != nil {
			return err
		}
	}
	return nil
}
