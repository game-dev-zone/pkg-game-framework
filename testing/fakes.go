// Package testing 提供 in-memory 的 Tx / Record 假實作，讓 GameLogic 單元測試
// 不需真正 Consul / Postgres 就能驅動 framework 的 lifecycle。
package testing

import (
	"context"
	"errors"
	"sync"

	commonv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/common/v1"
	recordv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/record/v1"
	txv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/tx/v1"
	"google.golang.org/grpc"
)

// ---------------------------------------------------------------------------
// FakeTx
// ---------------------------------------------------------------------------

// FakeTx 是 txv1.TxServiceClient 的 in-memory 實作。
// 以 idempotency_key 去重並保留每筆 Transfer。
type FakeTx struct {
	mu           sync.Mutex
	balances     map[string]int64
	frozen       map[string]int64
	transfers    map[string]*txv1.TransferResponse // keyed by idempotency_key
	TransferLog  []*txv1.TransferRequest
	ForceError   error // 設定後所有 Transfer 直接回此 error
}

func NewFakeTx() *FakeTx {
	return &FakeTx{
		balances:  make(map[string]int64),
		frozen:    make(map[string]int64),
		transfers: make(map[string]*txv1.TransferResponse),
	}
}

// Seed 預設某 user 餘額。
func (f *FakeTx) Seed(userID string, balance int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[userID] = balance
}

func (f *FakeTx) Transfer(ctx context.Context, req *txv1.TransferRequest, _ ...grpc.CallOption) (*txv1.TransferResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ForceError != nil {
		return nil, f.ForceError
	}
	if req.IdempotencyKey == "" {
		return nil, errors.New("fake_tx: idempotency_key required")
	}
	if cached, ok := f.transfers[req.IdempotencyKey]; ok {
		return cached, nil
	}
	f.TransferLog = append(f.TransferLog, req)

	// 套用每筆 entry 的資金變動（不鎖定正負平衡，純記錄）。
	for _, e := range req.Entries {
		switch e.Type {
		case txv1.TxType_TX_TYPE_FREEZE:
			f.balances[e.UserId] -= e.Amount
			f.frozen[e.UserId] += e.Amount
		case txv1.TxType_TX_TYPE_UNFREEZE:
			f.balances[e.UserId] += e.Amount
			f.frozen[e.UserId] -= e.Amount
		default:
			f.balances[e.UserId] += e.Amount
		}
	}

	// 回傳受影響錢包
	seen := map[string]struct{}{}
	wallets := make([]*commonv1.Wallet, 0)
	for _, e := range req.Entries {
		if _, ok := seen[e.UserId]; ok {
			continue
		}
		seen[e.UserId] = struct{}{}
		wallets = append(wallets, &commonv1.Wallet{
			UserId:   e.UserId,
			Balance:  f.balances[e.UserId],
			Frozen:   f.frozen[e.UserId],
			Currency: "CNY",
		})
	}
	resp := &txv1.TransferResponse{
		Ack:     &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK},
		Wallets: wallets,
	}
	f.transfers[req.IdempotencyKey] = resp
	return resp, nil
}

func (f *FakeTx) GetBalance(ctx context.Context, req *txv1.GetBalanceRequest, _ ...grpc.CallOption) (*txv1.GetBalanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &txv1.GetBalanceResponse{
		Ack: &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK},
		Wallet: &commonv1.Wallet{
			UserId:   req.UserId,
			Balance:  f.balances[req.UserId],
			Frozen:   f.frozen[req.UserId],
			Currency: "CNY",
		},
	}, nil
}

func (f *FakeTx) QueryRecords(ctx context.Context, req *txv1.QueryRecordsRequest, _ ...grpc.CallOption) (*txv1.QueryRecordsResponse, error) {
	return &txv1.QueryRecordsResponse{Ack: &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK}}, nil
}

// ---------------------------------------------------------------------------
// FakeRecord
// ---------------------------------------------------------------------------

type FakeRecord struct {
	mu       sync.Mutex
	records  []*recordv1.GameRecord
	byKey    map[string]*recordv1.GameRecord
	ForceErr error
}

func NewFakeRecord() *FakeRecord {
	return &FakeRecord{byKey: make(map[string]*recordv1.GameRecord)}
}

func (f *FakeRecord) WriteGameRecord(ctx context.Context, req *recordv1.WriteGameRecordRequest, _ ...grpc.CallOption) (*recordv1.WriteGameRecordResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ForceErr != nil {
		return nil, f.ForceErr
	}
	r := req.GetRecord()
	if existing, ok := f.byKey[r.IdempotencyKey]; ok {
		return &recordv1.WriteGameRecordResponse{
			Ack: &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK}, RecordId: existing.RecordId,
		}, nil
	}
	if r.RecordId == "" {
		r.RecordId = "rec-" + req.Record.IdempotencyKey
	}
	f.records = append(f.records, r)
	f.byKey[r.IdempotencyKey] = r
	return &recordv1.WriteGameRecordResponse{
		Ack: &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK}, RecordId: r.RecordId,
	}, nil
}

func (f *FakeRecord) QueryGameRecords(ctx context.Context, req *recordv1.QueryGameRecordsRequest, _ ...grpc.CallOption) (*recordv1.QueryGameRecordsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*recordv1.GameRecord, 0, len(f.records))
	for _, r := range f.records {
		if req.GameId != "" && r.GameId != req.GameId {
			continue
		}
		if req.RoomId != "" && r.RoomId != req.RoomId {
			continue
		}
		out = append(out, r)
	}
	return &recordv1.QueryGameRecordsResponse{
		Ack:     &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK},
		Records: out,
	}, nil
}

// Records 回傳目前保存的全部紀錄（read-only 快照）。
func (f *FakeRecord) Records() []*recordv1.GameRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*recordv1.GameRecord, len(f.records))
	copy(out, f.records)
	return out
}
