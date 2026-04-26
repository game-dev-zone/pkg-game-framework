// Package framework 是 Club No.8 遊戲服務的執行時框架。
//
// 協作者開發新遊戲時只需：
//   1. 實作 GameLogic interface（定義於此檔）。
//   2. 在 main 呼叫 framework.Run(ctx, cfg, logic)。
//
// 框架處理：Consul 註冊、ETCD session 綁定、gRPC server 與 health、
// Tx client 撥號、Record client 撥號、idempotency key 產生、結算後自動寫紀錄、
// 訂閱廣播、優雅關閉。
package framework

import (
	"context"
	"time"

	gamev1 "github.com/game-dev-zone/pkg-proto/gen/go/club/game/v1"
	recordv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/record/v1"
	txv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/tx/v1"
	"github.com/game-dev-zone/pkg-game-framework/room"
	"github.com/rs/zerolog"
)

// GameMeta 描述遊戲的靜態屬性，由 GameLogic.Meta() 回傳。
type GameMeta struct {
	GameID       string        // "ddz" / "niuniu" / "slots" / ...
	DefaultSeats int32         // CreateRoomRequest.MaxSeats 為 0 時採用
	TickInterval time.Duration // 0 禁用 OnTick；>0 啟動 tick goroutine 週期呼叫
}

// GameLogic 是遊戲開發者實作的核心 interface。
//
// 生命週期（以 PlaceBet 為例）：
//
//   1. gRPC client 呼叫 GameService.PlaceBet
//   2. 框架：檢查 room 存在 → Tx.BET（冪等 key: "<game_id>-bet-<room>-<user>-<amount>"）
//      → room.PlaceBet（更新 stake + 廣播 state）
//   3. 框架：呼叫 logic.OnPlaceBet（你可在這裡透過 room.WithGameState 更新自訂狀態）
//   4. 若 OnPlaceBet 回傳 error：框架發起補償性 Tx.REFUND，回復 client 錯誤
//
// 設計原則：
//   - GameLogic 的方法是「純決策」；錢與房間狀態由框架 orchestrate。
//   - 回傳 error 表示拒絕本次操作；框架會根據該階段做適當補償。
//   - 若需要直接呼叫 Tx 或 Record，用 ctx.Tx() / ctx.Record() escape hatch
//     （僅在預設 lifecycle 不夠用時）。
type GameLogic interface {
	Meta() GameMeta

	// OnCreateRoom 在框架建立 room 並完成 Tx.FREEZE（若 OwnerStake > 0）後呼叫。
	// 回傳 error 框架會補償 Tx.UNFREEZE 並移除 room。
	OnCreateRoom(ctx Context, req *gamev1.CreateRoomRequest, room *room.Room) error

	// OnEnterRoom 在框架呼叫 room.Enter（已完成座位加入 + snapshot 廣播）後呼叫。
	// 回傳 error 框架不會補償（座位已加入），由呼叫端處理。
	OnEnterRoom(ctx Context, req *gamev1.EnterRoomRequest, room *room.Room) error

	// OnPlaceBet 在框架完成 Tx.BET + room.PlaceBet 後呼叫。
	// 回傳 error 框架會發起補償性 Tx.REFUND。
	OnPlaceBet(ctx Context, req *gamev1.PlaceBetRequest, room *room.Room) error

	// OnSettle 是唯一由 GameLogic 主動回傳 Payouts 的 hook；框架會把 Payouts
	// 轉成 TxEntries 原子寫入 Tx，並背景寫入 Record。
	// 回傳 empty slice 代表「無資金變動」；回傳 error 則中止結算。
	OnSettle(ctx Context, req *gamev1.SettleRequest, room *room.Room) ([]*gamev1.Payout, error)

	// OnTick 若 Meta().TickInterval > 0 時，框架會依該間隔對每個 Room 呼叫。
	// 回傳 error 會觸發 room 關閉。用於牌局計時、動畫驅動等。
	OnTick(ctx Context, room *room.Room) error
}

// Context 於 GameLogic 方法中提供 framework 級的輔助。
// 繼承 context.Context 介面供取消與 deadline。
type Context interface {
	context.Context
	TraceID() string
	Logger() zerolog.Logger

	// Escape hatch：多數情況 GameLogic 不應直接呼叫。
	Tx() txv1.TxServiceClient
	Record() recordv1.RecordServiceClient
}

// SettlementMeta 是「可選」介面（interface segregation）。GameLogic 若實作此
// 介面，框架會在 Settle 完成後 fire-and-forget 額外做兩件事：
//   1. POST report-service /internal/reports/ingest（產 match_settlement read-model）
//   2. POST card-service /internal/cards/consume（依 RakeCard 從俱樂部庫存扣抽水）
//
// 對舊遊戲（DDZ 範例）保持向下相容：未實作此介面 = 維持原狀（只寫 Tx + Record）。
type SettlementMeta interface {
	// SettlementInfo 在每次 Settle 時被呼叫，請回傳該局所屬的 club / 抽水房卡數
	// / 該局起始時間（用於 report 對齊）。
	//
	// ClubID 為空字串 = 無歸屬俱樂部，框架會跳過 report+card 整合（仍寫 Tx +
	// Record）。RakeCard 為 0 = 不抽水。
	SettlementInfo(req *gamev1.SettleRequest, room *room.Room) SettlementMetaResult
}

// SettlementMetaResult 是 SettlementInfo 的回傳；零值代表「無 club、無抽水」。
type SettlementMetaResult struct {
	ClubID    string // UUID 格式；空字串 = 不做 report/card 整合
	RoundID   string // 可選；給 report.match_settlements.round_id
	RakeCard  int64  // 該局抽水房卡數（從 club_settings.card_per_room + rake_bps 推算）
	StartedAt time.Time
}
