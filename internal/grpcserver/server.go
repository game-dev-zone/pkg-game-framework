// Package grpcserver 實作 club.game.v1.GameService 的通用 gRPC handler。
//
// 所有五個 RPC（CreateRoom / EnterRoom / LeaveRoom / PlaceBet / Settle / Subscribe）
// 都由本 handler 處理：呼叫 Tx 與 Record、維護 ETCD session、
// 並在合適時機呼叫 GameLogic hook。
//
// 僅供 pkg-game-framework 內部使用。
package grpcserver

import (
	"context"
	"fmt"
	"time"

	commonv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/common/v1"
	gamev1 "github.com/game-dev-zone/pkg-proto/gen/go/club/game/v1"
	recordv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/record/v1"
	txv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/tx/v1"
	"github.com/game-dev-zone/pkg-game-framework/internal/cardclient"
	"github.com/game-dev-zone/pkg-game-framework/internal/reportclient"
	"github.com/game-dev-zone/pkg-game-framework/internal/session"
	"github.com/game-dev-zone/pkg-game-framework/internal/txclient"
	"github.com/game-dev-zone/pkg-game-framework/room"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// SettlementMetaInfo 是 framework 端 SettlementMeta 介面回傳的反序列化結構。
// grpcserver 不直接 import framework package（會循環），透過 LogicAdapter 提供。
type SettlementMetaInfo struct {
	ClubID    string
	RoundID   string
	RakeCard  int64
	StartedAt time.Time
}

// LogicAdapter 是 framework → logic 的最小介面。
// 避免 grpcserver 直接 import framework（會產生循環）。
// framework 在 Run() 中傳一個 adapter 進來。
type LogicAdapter interface {
	GameID() string
	DefaultSeats() int32

	OnCreateRoom(ctx FrameworkContext, req *gamev1.CreateRoomRequest, room *room.Room) error
	OnEnterRoom(ctx FrameworkContext, req *gamev1.EnterRoomRequest, room *room.Room) error
	OnPlaceBet(ctx FrameworkContext, req *gamev1.PlaceBetRequest, room *room.Room) error
	OnSettle(ctx FrameworkContext, req *gamev1.SettleRequest, room *room.Room) ([]*gamev1.Payout, error)

	// SettlementInfo 為「可選」hook：framework adapter 在 GameLogic 實作
	// SettlementMeta 介面時回傳；否則回傳 (info, false)。
	// info.ClubID 為空字串 = 跳過 report+card 整合。
	SettlementInfo(req *gamev1.SettleRequest, room *room.Room) (SettlementMetaInfo, bool)
}

// FrameworkContext 是 framework.Context 的結構映射（由 framework package 實作）。
// 此處作為 type alias 的佔位；grpcserver 在呼叫 logic 時傳入。
type FrameworkContext interface {
	context.Context
	TraceID() string
	Logger() zerolog.Logger
	Tx() txv1.TxServiceClient
	Record() recordv1.RecordServiceClient
}

// NewContext 允許外部構造 FrameworkContext 實例（由 framework package 提供）。
type NewContextFn func(ctx context.Context, traceID string) FrameworkContext

// Server 是 GameService 的 gRPC 實作。
type Server struct {
	gamev1.UnimplementedGameServiceServer

	mgr           *room.Manager
	sess          *session.Store
	tx            *txclient.Client
	record        recordv1.RecordServiceClient // 可為 nil（本機 dev 不起 record）
	report        *reportclient.Client         // 可為 nil（dev 預設不啟用整合）
	card          *cardclient.Client           // 可為 nil
	keys          txclient.Keys
	logic         LogicAdapter
	log           zerolog.Logger
	newCtx        NewContextFn
	recordTimeout time.Duration
}

func New(
	mgr *room.Manager,
	sess *session.Store,
	tx *txclient.Client,
	record recordv1.RecordServiceClient,
	report *reportclient.Client,
	card *cardclient.Client,
	logic LogicAdapter,
	newCtx NewContextFn,
	recordTimeout time.Duration,
	log zerolog.Logger,
) *Server {
	return &Server{
		mgr:           mgr,
		sess:          sess,
		tx:            tx,
		record:        record,
		report:        report,
		card:          card,
		keys:          txclient.Keys{GameID: logic.GameID()},
		logic:         logic,
		log:           log.With().Str("component", "grpcserver").Logger(),
		newCtx:        newCtx,
		recordTimeout: recordTimeout,
	}
}

// ---------------------------------------------------------------------------
// RPC handlers
// ---------------------------------------------------------------------------

func (s *Server) CreateRoom(ctx context.Context, req *gamev1.CreateRoomRequest) (*gamev1.CreateRoomResponse, error) {
	traceID := uuid.NewString()
	fctx := s.newCtx(ctx, traceID)

	r, err := s.mgr.Create(req.UserId, s.logic.GameID(), req.Ante, s.resolveSeats(req.MaxSeats))
	if err != nil {
		return errResp[gamev1.CreateRoomResponse](commonv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()), nil
	}

	// 凍結莊家押金（若有）
	if req.OwnerStake > 0 {
		_, err := s.tx.Raw().Transfer(ctx, &txv1.TransferRequest{
			IdempotencyKey: s.keys.Create(r.ID),
			Entries: []*txv1.TxEntry{{
				UserId: req.UserId, Type: txv1.TxType_TX_TYPE_FREEZE,
				Amount: req.OwnerStake, RefRoomId: r.ID, RefGameId: s.logic.GameID(),
				Memo: "owner stake", TraceId: traceID,
			}},
		})
		if err != nil {
			s.mgr.Remove(r.ID)
			return nil, fmt.Errorf("tx freeze: %w", err)
		}
	}

	if err := s.logic.OnCreateRoom(fctx, req, r); err != nil {
		// 補償：若已凍結，發反向 UNFREEZE 並移除 room
		if req.OwnerStake > 0 {
			_, _ = s.tx.Raw().Transfer(ctx, &txv1.TransferRequest{
				IdempotencyKey: s.keys.CreateCompensate(r.ID),
				Entries: []*txv1.TxEntry{{
					UserId: req.UserId, Type: txv1.TxType_TX_TYPE_UNFREEZE,
					Amount: req.OwnerStake, RefRoomId: r.ID, RefGameId: s.logic.GameID(),
					Memo: "create compensate", TraceId: traceID,
				}},
			})
		}
		s.mgr.Remove(r.ID)
		return errResp[gamev1.CreateRoomResponse](commonv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()), nil
	}

	if err := s.sess.Bind(ctx, r.ID); err != nil {
		s.log.Warn().Err(err).Msg("etcd bind failed; room still usable locally")
	}

	return &gamev1.CreateRoomResponse{
		Ack:    &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK, TraceId: traceID},
		RoomId: r.ID,
	}, nil
}

func (s *Server) EnterRoom(ctx context.Context, req *gamev1.EnterRoomRequest) (*gamev1.EnterRoomResponse, error) {
	traceID := uuid.NewString()
	fctx := s.newCtx(ctx, traceID)

	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		return errResp[gamev1.EnterRoomResponse](commonv1.ErrorCode_ERROR_CODE_ROOM_NOT_FOUND, "room not found"), nil
	}
	snap, err := r.Enter(req.UserId)
	if err != nil {
		return errResp[gamev1.EnterRoomResponse](codeFromRoomErr(err), err.Error()), nil
	}
	if err := s.logic.OnEnterRoom(fctx, req, r); err != nil {
		return errResp[gamev1.EnterRoomResponse](commonv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()), nil
	}
	return &gamev1.EnterRoomResponse{
		Ack:      &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK, TraceId: traceID},
		Snapshot: snap,
	}, nil
}

func (s *Server) LeaveRoom(ctx context.Context, req *gamev1.LeaveRoomRequest) (*gamev1.LeaveRoomResponse, error) {
	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		return errResp[gamev1.LeaveRoomResponse](commonv1.ErrorCode_ERROR_CODE_ROOM_NOT_FOUND, "room not found"), nil
	}
	if err := r.Leave(req.UserId); err != nil {
		return errResp[gamev1.LeaveRoomResponse](codeFromRoomErr(err), err.Error()), nil
	}
	return &gamev1.LeaveRoomResponse{Ack: okAck()}, nil
}

func (s *Server) PlaceBet(ctx context.Context, req *gamev1.PlaceBetRequest) (*gamev1.PlaceBetResponse, error) {
	traceID := uuid.NewString()
	fctx := s.newCtx(ctx, traceID)

	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		return errResp[gamev1.PlaceBetResponse](commonv1.ErrorCode_ERROR_CODE_ROOM_NOT_FOUND, "room not found"), nil
	}

	// 先 Tx.BET，成功後更新 room state（先有帳本再有狀態）。
	resp, err := s.tx.Raw().Transfer(ctx, &txv1.TransferRequest{
		IdempotencyKey: s.keys.Bet(req.RoomId, req.UserId, req.Amount),
		Entries: []*txv1.TxEntry{{
			UserId: req.UserId, Type: txv1.TxType_TX_TYPE_BET, Amount: -req.Amount,
			RefRoomId: req.RoomId, RefGameId: s.logic.GameID(), TraceId: traceID,
		}},
	})
	if err != nil {
		return nil, err
	}
	if resp.Ack.Code != commonv1.ErrorCode_ERROR_CODE_OK {
		return &gamev1.PlaceBetResponse{Ack: resp.Ack}, nil
	}

	if err := r.PlaceBet(req.UserId, req.Amount); err != nil {
		s.refundBet(ctx, req, traceID)
		return errResp[gamev1.PlaceBetResponse](codeFromRoomErr(err), err.Error()), nil
	}

	if err := s.logic.OnPlaceBet(fctx, req, r); err != nil {
		_ = r.RefundBet(req.UserId, req.Amount)
		s.refundBet(ctx, req, traceID)
		return errResp[gamev1.PlaceBetResponse](commonv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()), nil
	}

	var newBalance int64
	if len(resp.Wallets) > 0 {
		newBalance = resp.Wallets[0].Balance
	}
	return &gamev1.PlaceBetResponse{
		Ack:        &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK, TraceId: traceID},
		NewBalance: newBalance,
	}, nil
}

func (s *Server) Settle(ctx context.Context, req *gamev1.SettleRequest) (*gamev1.SettleResponse, error) {
	traceID := uuid.NewString()
	fctx := s.newCtx(ctx, traceID)

	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		return errResp[gamev1.SettleResponse](commonv1.ErrorCode_ERROR_CODE_ROOM_NOT_FOUND, "room not found"), nil
	}
	startedAt := time.Now().Add(-time.Minute) // 取代為真實 room start 時間若有持久化
	if err := r.Settle(); err != nil {
		return errResp[gamev1.SettleResponse](codeFromRoomErr(err), err.Error()), nil
	}

	payouts, err := s.logic.OnSettle(fctx, req, r)
	if err != nil {
		return errResp[gamev1.SettleResponse](commonv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()), nil
	}

	// 把 Payouts 轉 TxEntries。正數 → SETTLE_WIN，負數 → SETTLE_LOSS，零略過。
	entries := make([]*txv1.TxEntry, 0, len(payouts))
	totalPot := int64(0)
	for _, p := range payouts {
		if p.Delta == 0 {
			continue
		}
		txType := txv1.TxType_TX_TYPE_SETTLE_WIN
		if p.Delta < 0 {
			txType = txv1.TxType_TX_TYPE_SETTLE_LOSS
			totalPot += -p.Delta
		}
		entries = append(entries, &txv1.TxEntry{
			UserId: p.UserId, Type: txType, Amount: p.Delta,
			RefRoomId: req.RoomId, RefGameId: s.logic.GameID(),
			Memo: p.Reason, TraceId: traceID,
		})
	}
	if len(entries) > 0 {
		if _, err := s.tx.Raw().Transfer(ctx, &txv1.TransferRequest{
			IdempotencyKey: s.keys.Settle(req.RoomId, ""),
			Entries:        entries,
		}); err != nil {
			return nil, fmt.Errorf("tx settle: %w", err)
		}
	}

	r.BroadcastPayout(payouts)
	s.writeRecord(ctx, req.RoomId, payouts, startedAt, totalPot, traceID)
	s.notifyReportAndRake(ctx, req, payouts, startedAt, totalPot, traceID)
	s.mgr.Remove(req.RoomId)
	_ = s.sess.Unbind(ctx, req.RoomId)

	return &gamev1.SettleResponse{
		Ack:     &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK, TraceId: traceID},
		Payouts: payouts,
	}, nil
}

// Subscribe 以 server-stream 推送 SubscribeResponse。
func (s *Server) Subscribe(req *gamev1.SubscribeRequest, stream gamev1.GameService_SubscribeServer) error {
	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		return fmt.Errorf("room not found: %s", req.RoomId)
	}
	ch, cancel, err := r.Subscribe(req.UserId)
	if err != nil {
		return err
	}
	defer cancel()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 內部輔助
// ---------------------------------------------------------------------------

func (s *Server) resolveSeats(v int32) int32 {
	if v > 0 {
		return v
	}
	if d := s.logic.DefaultSeats(); d > 0 {
		return d
	}
	return 3
}

func (s *Server) refundBet(ctx context.Context, req *gamev1.PlaceBetRequest, traceID string) {
	if _, err := s.tx.Raw().Transfer(ctx, &txv1.TransferRequest{
		IdempotencyKey: s.keys.BetRefund(req.RoomId, req.UserId, req.Amount),
		Entries: []*txv1.TxEntry{{
			UserId: req.UserId, Type: txv1.TxType_TX_TYPE_REFUND, Amount: req.Amount,
			RefRoomId: req.RoomId, RefGameId: s.logic.GameID(),
			Memo: "bet compensate", TraceId: traceID,
		}},
	}); err != nil {
		s.log.Error().Err(err).Str("room_id", req.RoomId).Str("user_id", req.UserId).
			Int64("amount", req.Amount).Msg("bet refund tx failed")
	}
}

// writeRecord 以 2 秒超時背景寫 record；失敗不影響 Settle 回應。
func (s *Server) writeRecord(
	parent context.Context,
	roomID string,
	payouts []*gamev1.Payout,
	startedAt time.Time,
	totalPot int64,
	traceID string,
) {
	if s.record == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, s.recordTimeout)
	defer cancel()
	now := time.Now()
	_, err := s.record.WriteGameRecord(ctx, &recordv1.WriteGameRecordRequest{
		Record: &recordv1.GameRecord{
			GameId:         s.logic.GameID(),
			RoomId:         roomID,
			StartedAt:      startedAt.Unix(),
			SettledAt:      now.Unix(),
			TotalPot:       totalPot,
			Payouts:        payouts,
			IdempotencyKey: s.keys.Settle(roomID, ""),
			TraceId:        traceID,
		},
	})
	if err != nil {
		s.log.Error().Err(err).Str("room_id", roomID).
			Str("idempotency_key", s.keys.Settle(roomID, "")).
			Msg("record write failed; run back-office replay from this log")
	}
}

// notifyReportAndRake 在 Settle 完成後 fire-and-forget：
//   1. 若 GameLogic 實作 SettlementMeta 且 ClubID != "" → POST report-service ingest
//   2. 若 SettlementMeta.RakeCard > 0 且 card client 配置 → POST card-service consume
// 失敗只 log warn，不影響 client 已收到的成功 Settle 回應。
func (s *Server) notifyReportAndRake(
	parent context.Context,
	req *gamev1.SettleRequest,
	payouts []*gamev1.Payout,
	startedAt time.Time,
	totalPot int64,
	traceID string,
) {
	r, ok := s.mgr.Get(req.RoomId)
	if !ok {
		// settle 後 room 已 remove；改不檢查（payouts 帶足夠資訊）
	}
	info, has := s.logic.SettlementInfo(req, r)
	if !has || info.ClubID == "" {
		return
	}

	// 1. report-service ingest（背景跑，不阻塞）
	if s.report != nil {
		go func(info SettlementMetaInfo) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			lite := make([]reportclient.PayoutLite, 0, len(payouts))
			for _, p := range payouts {
				lite = append(lite, reportclient.PayoutLite{
					UserID: p.UserId, Delta: p.Delta, Reason: p.Reason,
				})
			}
			clubUUID, err := uuid.Parse(info.ClubID)
			if err != nil {
				s.log.Warn().Str("club_id", info.ClubID).Err(err).Msg("report ingest: bad club_id (not UUID)")
				return
			}
			// game_record_id 與 record-service 寫入的 ID 對齊：framework 用
			// idempotency_key 寫 record；read-model 端只需有「同 room 同 settle 的唯一 ID」即可
			// → 這裡用 deterministic UUID 從 idempotency_key 推（v5 namespace）
			recordIDemulated := uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.keys.Settle(req.RoomId, "")))
			started := info.StartedAt
			if started.IsZero() {
				started = startedAt
			}
			err = s.report.Ingest(ctx, reportclient.IngestRequest{
				GameRecordID: recordIDemulated,
				ClubID:       clubUUID,
				GameID:       s.logic.GameID(),
				RoomID:       req.RoomId,
				RoundID:      info.RoundID,
				StartedAt:    started,
				SettledAt:    time.Now(),
				TotalPot:     totalPot,
				RakeCard:     info.RakeCard,
				PerUser:      reportclient.PayoutsToPerUser(lite),
			})
			if err != nil {
				s.log.Warn().Err(err).Str("room_id", req.RoomId).Msg("report ingest failed")
			}
		}(info)
	}

	// 2. card-service consume rake（背景跑）
	if s.card != nil && info.RakeCard > 0 {
		go func(info SettlementMetaInfo) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := s.card.Consume(ctx, cardclient.ConsumeRequest{
				FromOwnerType:  "CLUB",
				FromOwnerID:    info.ClubID,
				Quantity:       info.RakeCard,
				Reason:         "RAKE",
				RefRoomID:      req.RoomId,
				RefGameID:      s.logic.GameID(),
				IdempotencyKey: fmt.Sprintf("%s-rake-%s", s.logic.GameID(), req.RoomId),
				TraceID:        traceID,
				Note:           "auto rake from framework Settle hook",
			})
			if err != nil {
				s.log.Warn().Err(err).Str("club_id", info.ClubID).Int64("rake", info.RakeCard).
					Msg("card consume rake failed")
			}
		}(info)
	}
}
