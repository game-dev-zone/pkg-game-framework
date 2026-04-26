// Package cardclient 是 framework 內部的 HTTP client，
// 在 Settle 完成後從俱樂部庫存扣抽水（RAKE）— 平台房卡收入引擎。
package cardclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string // 例：http://127.0.0.1:9801
	Timeout time.Duration
}

func New(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	return &Client{BaseURL: baseURL, Timeout: timeout}
}

type ConsumeRequest struct {
	FromOwnerType  string `json:"from_owner_type"` // CLUB / AGENT / USER；rake 一般走 CLUB
	FromOwnerID    string `json:"from_owner_id"`
	Quantity       int64  `json:"quantity"`
	Reason         string `json:"reason,omitempty"` // RAKE / ROOM_CONSUME
	RefRoomID      string `json:"ref_room_id,omitempty"`
	RefGameID      string `json:"ref_game_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	TraceID        string `json:"trace_id,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Consume：呼叫 card-service /internal/cards/consume 扣俱樂部 RAKE 房卡。
// 失敗回傳 error；framework 端決定要 log warn 還是回 client 錯誤
// （MVP 設計為 fire-and-forget，平台收入有少算情境會由風控 + 對帳發現）。
func (c *Client) Consume(parent context.Context, req ConsumeRequest) error {
	if c.BaseURL == "" {
		return fmt.Errorf("card base url empty")
	}
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/internal/cards/consume", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("card consume status %d", resp.StatusCode)
	}
	return nil
}
