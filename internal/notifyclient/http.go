// Package notifyclient 是 framework 內部的 HTTP client，在 Settle 完成後對
// 大額 payout winner 推「中大獎」訊息。
//
// 與 cardclient 同模式：fire-and-forget，service-to-service 用 INTERNAL_SERVICE_SECRET
// HMAC token；失敗只 log warn，不影響 Settle 回應。
package notifyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	internalauth "github.com/game-dev-zone/pkg-internal-auth"
)

type Client struct {
	BaseURL string
	Timeout time.Duration
}

func New(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	return &Client{BaseURL: baseURL, Timeout: timeout}
}

// SendPayoutWin 推一條「中大獎」給單一玩家。
//   gameID/roomID 進 idempotency_key；同房同玩家重打不重複。
func (c *Client) SendPayoutWin(parent context.Context, userID, gameID, roomID string, delta int64) error {
	if c.BaseURL == "" {
		return fmt.Errorf("notify base url empty")
	}
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()

	payload := map[string]any{
		"message": map[string]any{
			"title":   "中大獎",
			"body":    fmt.Sprintf("您在 %s 贏得 %d ！", gameID, delta),
			"channel": "transaction",
			"data": map[string]string{
				"game_id":  gameID,
				"room_id":  roomID,
				"amount":   fmt.Sprintf("%d", delta),
				"deeplink": "club8://wallet",
			},
		},
		"target":          map[string]any{"user_id": userID},
		"idempotency_key": fmt.Sprintf("payout-win-%s-%s-%s", gameID, roomID, userID),
		"trigger_source":  "service:game-framework:payout",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/internal/notify/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := os.Getenv("INTERNAL_SERVICE_SECRET"); secret != "" {
		if tok, err := internalauth.SignNow(secret, "game-framework"); err == nil {
			req.Header.Set(internalauth.Header, tok)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notify send status %d", resp.StatusCode)
	}
	return nil
}
