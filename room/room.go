// Package room 提供 game-agnostic 的 Room actor 與 Manager，
// 供 pkg-game-framework 內部與 GameLogic 共同使用。
//
// 每個 Room 維護自身的 seats、state、subscribers，並以 mutex 保護；
// GameLogic 想要寫自訂遊戲狀態時，透過 Room.WithGameState 安全地在鎖內修改。
package room

import (
	"errors"
	"fmt"
	"sync"
	"time"

	gamev1 "github.com/club8/pkg-proto/gen/go/club/game/v1"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

type Manager struct {
	log   zerolog.Logger
	mu    sync.RWMutex
	rooms map[string]*Room
}

func NewManager(log zerolog.Logger) *Manager {
	return &Manager{
		log:   log.With().Str("component", "room.manager").Logger(),
		rooms: make(map[string]*Room),
	}
}

// Create 建立房間，房主自動坐 0 號位。maxSeats ≤ 0 時呼叫端負責補預設值。
func (m *Manager) Create(ownerID, gameID string, ante int64, maxSeats int32) (*Room, error) {
	if maxSeats <= 0 {
		maxSeats = 3
	}
	rid := uuid.NewString()
	r := newRoom(rid, ownerID, gameID, ante, int(maxSeats), m.log)

	m.mu.Lock()
	m.rooms[rid] = r
	m.mu.Unlock()

	go r.run()
	return r, nil
}

func (m *Manager) Get(roomID string) (*Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rooms[roomID]
	return r, ok
}

// ForEach 以讀鎖掃描所有房間；fn 應快速返回，避免阻塞新房建立。
func (m *Manager) ForEach(fn func(*Room)) {
	m.mu.RLock()
	snapshot := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		snapshot = append(snapshot, r)
	}
	m.mu.RUnlock()
	for _, r := range snapshot {
		fn(r)
	}
}

func (m *Manager) Remove(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[roomID]; ok {
		r.Close()
		delete(m.rooms, roomID)
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rooms {
		r.Close()
	}
	m.rooms = nil
}

// ---------------------------------------------------------------------------
// Room
// ---------------------------------------------------------------------------

type Room struct {
	ID       string
	OwnerID  string
	GameID   string
	Ante     int64
	MaxSeats int
	State    gamev1.GameState

	log zerolog.Logger

	mu          sync.RWMutex
	seats       []seat
	subscribers map[string]chan *gamev1.SubscribeResponse
	gameState   map[string]any // GameLogic 自訂狀態；由 WithGameState 安全存取

	done chan struct{}
}

type seat struct {
	index  int
	userID string
	stake  int64
	owner  bool
}

func newRoom(id, ownerID, gameID string, ante int64, max int, log zerolog.Logger) *Room {
	r := &Room{
		ID:          id,
		OwnerID:     ownerID,
		GameID:      gameID,
		Ante:        ante,
		MaxSeats:    max,
		State:       gamev1.GameState_GAME_STATE_WAITING,
		log:         log.With().Str("room_id", id).Logger(),
		seats:       make([]seat, 0, max),
		subscribers: make(map[string]chan *gamev1.SubscribeResponse),
		gameState:   make(map[string]any),
		done:        make(chan struct{}),
	}
	// 房主自動坐上 0 號位
	r.seats = append(r.seats, seat{index: 0, userID: ownerID, owner: true})
	return r
}

func (r *Room) run() {
	// 保留 goroutine 以便未來接入牌局計時器；目前僅阻塞至 Close。
	<-r.done
}

func (r *Room) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.State == gamev1.GameState_GAME_STATE_CLOSED {
		return
	}
	r.State = gamev1.GameState_GAME_STATE_CLOSED
	for _, ch := range r.subscribers {
		close(ch)
	}
	r.subscribers = nil
	close(r.done)
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	ErrRoomFull    = errors.New("room full")
	ErrAlreadyJoin = errors.New("already in room")
	ErrNotInRoom   = errors.New("user not in room")
	ErrNotOwner    = errors.New("not room owner")
	ErrWrongState  = errors.New("wrong game state")
)

// ---------------------------------------------------------------------------
// Public ops
// ---------------------------------------------------------------------------

func (r *Room) Enter(userID string) (*gamev1.RoomSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State == gamev1.GameState_GAME_STATE_CLOSED {
		return nil, ErrWrongState
	}
	for _, s := range r.seats {
		if s.userID == userID {
			return nil, ErrAlreadyJoin
		}
	}
	if len(r.seats) >= r.MaxSeats {
		return nil, ErrRoomFull
	}
	r.seats = append(r.seats, seat{index: len(r.seats), userID: userID})
	snap := r.snapshotLocked()
	r.broadcastLocked(&gamev1.SubscribeResponse{
		RoomId:  r.ID,
		Ts:      time.Now().Unix(),
		Payload: &gamev1.SubscribeResponse_Snapshot{Snapshot: snap},
	})
	return snap, nil
}

func (r *Room) Leave(userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i, s := range r.seats {
		if s.userID == userID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotInRoom
	}
	r.seats = append(r.seats[:idx], r.seats[idx+1:]...)
	for i := range r.seats {
		r.seats[i].index = i
	}
	snap := r.snapshotLocked()
	r.broadcastLocked(&gamev1.SubscribeResponse{
		RoomId:  r.ID,
		Ts:      time.Now().Unix(),
		Payload: &gamev1.SubscribeResponse_Snapshot{Snapshot: snap},
	})
	return nil
}

func (r *Room) PlaceBet(userID string, amount int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != gamev1.GameState_GAME_STATE_WAITING &&
		r.State != gamev1.GameState_GAME_STATE_PLAYING {
		return ErrWrongState
	}
	for i := range r.seats {
		if r.seats[i].userID == userID {
			r.seats[i].stake += amount
			if r.State == gamev1.GameState_GAME_STATE_WAITING {
				r.State = gamev1.GameState_GAME_STATE_PLAYING
			}
			r.broadcastLocked(&gamev1.SubscribeResponse{
				RoomId:  r.ID,
				Ts:      time.Now().Unix(),
				Payload: &gamev1.SubscribeResponse_StateChanged{StateChanged: r.State},
			})
			return nil
		}
	}
	return ErrNotInRoom
}

// RefundBet 是 PlaceBet 的補償操作：當 GameLogic.OnPlaceBet 失敗時呼叫，
// 把 stake 退回（但不觸發 state 回退，因為 WAITING→PLAYING 可能已被他人觸發）。
func (r *Room) RefundBet(userID string, amount int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.seats {
		if r.seats[i].userID == userID {
			r.seats[i].stake -= amount
			if r.seats[i].stake < 0 {
				r.seats[i].stake = 0
			}
			return nil
		}
	}
	return ErrNotInRoom
}

// Settle 將房間標記為 SETTLING。實際的 Tx.Transfer 與 Payout 廣播由框架負責。
func (r *Room) Settle() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.State != gamev1.GameState_GAME_STATE_PLAYING {
		return ErrWrongState
	}
	r.State = gamev1.GameState_GAME_STATE_SETTLING
	r.broadcastLocked(&gamev1.SubscribeResponse{
		RoomId:  r.ID,
		Ts:      time.Now().Unix(),
		Payload: &gamev1.SubscribeResponse_StateChanged{StateChanged: r.State},
	})
	return nil
}

func (r *Room) BroadcastPayout(payouts []*gamev1.Payout) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().Unix()
	for _, p := range payouts {
		r.broadcastLocked(&gamev1.SubscribeResponse{
			RoomId:  r.ID,
			Ts:      now,
			Payload: &gamev1.SubscribeResponse_Payout{Payout: p},
		})
	}
}

// Subscribe 回傳訂閱 channel + cancel；主調用方需在 gateway 端持續讀。
func (r *Room) Subscribe(userID string) (<-chan *gamev1.SubscribeResponse, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.State == gamev1.GameState_GAME_STATE_CLOSED {
		return nil, nil, ErrWrongState
	}
	subID := fmt.Sprintf("%s-%s", userID, uuid.NewString()[:8])
	ch := make(chan *gamev1.SubscribeResponse, 32)
	r.subscribers[subID] = ch

	snap := r.snapshotLocked()
	select {
	case ch <- &gamev1.SubscribeResponse{
		RoomId:  r.ID,
		Ts:      time.Now().Unix(),
		Payload: &gamev1.SubscribeResponse_Snapshot{Snapshot: snap},
	}:
	default:
	}

	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subscribers[subID]; ok {
			delete(r.subscribers, subID)
			close(c)
		}
	}
	return ch, cancel, nil
}

// Snapshot 取得當前 room 的快照（read-only）。
func (r *Room) Snapshot() *gamev1.RoomSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotLocked()
}

func (r *Room) Owner() string { return r.OwnerID }

// WithGameState 在 room mutex 內原子地讀寫 GameLogic 的自訂遊戲狀態。
//
// 範例（DDZ 儲存當前出牌玩家索引）：
//
//	room.WithGameState(func(gs map[string]any) error {
//	    gs["active_seat"] = nextIdx
//	    return nil
//	})
//
// fn 的錯誤會原樣回傳；fn 執行期間 Room 的其他 Public ops（Enter/PlaceBet/…）
// 都會被 block，故 fn 內不可呼叫 Room 的其他方法（會 deadlock）。
func (r *Room) WithGameState(fn func(gs map[string]any) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(r.gameState)
}

// ReadGameState 讀取自訂狀態的 read-only 快照（淺複製）。
func (r *Room) ReadGameState(key string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.gameState[key]
	return v, ok
}

// ---------------------------------------------------------------------------
// internal helpers (caller holds r.mu)
// ---------------------------------------------------------------------------

func (r *Room) snapshotLocked() *gamev1.RoomSnapshot {
	seats := make([]*gamev1.Seat, len(r.seats))
	for i, s := range r.seats {
		seats[i] = &gamev1.Seat{
			Index:   int32(s.index),
			UserId:  s.userID,
			Stake:   s.stake,
			IsOwner: s.owner,
		}
	}
	return &gamev1.RoomSnapshot{
		RoomId:   r.ID,
		OwnerId:  r.OwnerID,
		GameId:   r.GameID,
		Seats:    seats,
		State:    r.State,
		Ante:     r.Ante,
		MaxSeats: int32(r.MaxSeats),
	}
}

func (r *Room) broadcastLocked(ev *gamev1.SubscribeResponse) {
	for id, ch := range r.subscribers {
		select {
		case ch <- ev:
		default:
			// 訂閱者 queue 滿了；移除避免阻塞
			delete(r.subscribers, id)
			close(ch)
			r.log.Warn().Str("sub", id).Msg("slow subscriber dropped")
		}
	}
}

// SeatStake 取得某 user 當前 stake（read-only snapshot）。
func (r *Room) SeatStake(userID string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.seats {
		if s.userID == userID {
			return s.stake, true
		}
	}
	return 0, false
}

// SeatStakes 以 (user_id → stake) 回傳所有座位的當前押注快照。
func (r *Room) SeatStakes() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int64, len(r.seats))
	for _, s := range r.seats {
		out[s.userID] = s.stake
	}
	return out
}
