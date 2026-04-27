// Package reportclient 是 framework 內部的 HTTP client，負責在 Settle 完成後
// 把該局結果 POST 到 report-service 產 match_settlement read-model。
//
// 為何 HTTP 而非 gRPC：report-service 的 ingest endpoint 走 HTTP/JSON
// （與 admin-api、loyalty 等內部 endpoint 一致）；framework 既有 gRPC 是
// 走 record service 寫 game_records。兩條路徑共存：record 寫真實源頭審計，
// report 寫 read-model 給俱樂部 drill-down。
package reportclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	internalauth "github.com/game-dev-zone/pkg-internal-auth"
)

type Client struct {
	BaseURL string // 例：http://127.0.0.1:10001
	Timeout time.Duration
}

func New(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	return &Client{BaseURL: baseURL, Timeout: timeout}
}

type ingestPerUser struct {
	UserID     uuid.UUID `json:"user_id"`
	DeltaChips int64     `json:"delta_chips"`
	Reason     string    `json:"reason,omitempty"`
}

type IngestRequest struct {
	GameRecordID uuid.UUID         `json:"game_record_id"` // 對應 record service 寫入的 record_id
	ClubID       uuid.UUID         `json:"club_id"`
	GameID       string            `json:"game_id"`
	RoomID       string            `json:"room_id"`
	RoundID      string            `json:"round_id,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	SettledAt    time.Time         `json:"settled_at"`
	TotalPot     int64             `json:"total_pot"`
	RakeCard     int64             `json:"rake_card,omitempty"`
	PerUser      []ingestPerUser   `json:"per_user"`
}

// Ingest fire-and-forget；錯誤回傳但不阻塞 Settle。
func (c *Client) Ingest(parent context.Context, req IngestRequest) error {
	if c.BaseURL == "" {
		return fmt.Errorf("report base url empty")
	}
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/internal/reports/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// service-to-service token；report-service /internal 群組 phase H 後要求 token。
	// secret 為空時不加 → middleware 退化為 pass-through（dev 友好；prod 必填）
	if secret := os.Getenv("INTERNAL_SERVICE_SECRET"); secret != "" {
		if tok, err := internalauth.SignNow(secret, "game-framework"); err == nil {
			httpReq.Header.Set(internalauth.Header, tok)
		}
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("report ingest status %d", resp.StatusCode)
	}
	return nil
}

// PerUser helper：把 framework 自己的 user_id (string) + delta 包成 IngestRequest 用的格式。
type PayoutLite struct {
	UserID string
	Delta  int64
	Reason string
}

func PayoutsToPerUser(payouts []PayoutLite) []ingestPerUser {
	out := make([]ingestPerUser, 0, len(payouts))
	for _, p := range payouts {
		uid, err := uuid.Parse(p.UserID)
		if err != nil {
			continue // 跳過非 UUID 格式（如 dev seed alice/bob）
		}
		out = append(out, ingestPerUser{
			UserID: uid, DeltaChips: p.Delta, Reason: p.Reason,
		})
	}
	return out
}
