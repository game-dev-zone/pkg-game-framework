package txclient

import "fmt"

// Keys 根據 game_id 為各 lifecycle 階段產生 idempotency_key。
//
// 格式（全部以 "<game_id>-" 為首）：
//
//	create-<room_id>             凍結莊家押金的 Tx.FREEZE
//	create-compensate-<room_id>  CreateRoom 失敗時的 Tx.UNFREEZE
//	bet-<room_id>-<user_id>-<amount>        一次 PlaceBet 的 Tx.BET
//	bet-refund-<room_id>-<user_id>-<amount> PlaceBet 失敗的補償性 Tx.REFUND
//	settle-<room_id>                        結算的 Tx.Transfer（多筆 entries）
//	settle-<room_id>-<round_id>             多局類遊戲的第 N 局結算
//
// 注意：framework 在 DDZ 遷移前使用無前綴的 key（"create-"/"bet-"/"settle-"），
// 遷移後一律加 game_id 前綴以便跨遊戲除錯。
type Keys struct {
	GameID string
}

func (k Keys) Create(roomID string) string {
	return fmt.Sprintf("%s-create-%s", k.GameID, roomID)
}

func (k Keys) CreateCompensate(roomID string) string {
	return fmt.Sprintf("%s-create-compensate-%s", k.GameID, roomID)
}

func (k Keys) Bet(roomID, userID string, amount int64) string {
	return fmt.Sprintf("%s-bet-%s-%s-%d", k.GameID, roomID, userID, amount)
}

func (k Keys) BetRefund(roomID, userID string, amount int64) string {
	return fmt.Sprintf("%s-bet-refund-%s-%s-%d", k.GameID, roomID, userID, amount)
}

func (k Keys) Settle(roomID, roundID string) string {
	if roundID == "" {
		return fmt.Sprintf("%s-settle-%s", k.GameID, roomID)
	}
	return fmt.Sprintf("%s-settle-%s-%s", k.GameID, roomID, roundID)
}
