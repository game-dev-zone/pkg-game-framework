package room

import (
	"testing"

	gamev1 "github.com/club8/pkg-proto/gen/go/club/game/v1"
	"github.com/rs/zerolog"
)

func newTestManager() *Manager {
	return NewManager(zerolog.Nop())
}

func TestManager_CreateAndEnter(t *testing.T) {
	m := newTestManager()
	r, err := m.Create("alice", "ddz", 100, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.Enter("bob"); err != nil {
		t.Fatalf("enter bob: %v", err)
	}
	if _, err := r.Enter("carol"); err != nil {
		t.Fatalf("enter carol: %v", err)
	}
	// 第 4 人超出 max_seats=3
	if _, err := r.Enter("dave"); err != ErrRoomFull {
		t.Fatalf("want ErrRoomFull, got %v", err)
	}
}

func TestManager_EnterTwiceRejected(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	if _, err := r.Enter("bob"); err != nil {
		t.Fatalf("enter: %v", err)
	}
	if _, err := r.Enter("bob"); err != ErrAlreadyJoin {
		t.Fatalf("want ErrAlreadyJoin, got %v", err)
	}
}

func TestRoom_PlaceBetAndSettle(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	_, _ = r.Enter("bob")

	if err := r.PlaceBet("alice", 50); err != nil {
		t.Fatalf("placebet alice: %v", err)
	}
	if r.State != gamev1.GameState_GAME_STATE_PLAYING {
		t.Fatalf("want PLAYING, got %v", r.State)
	}
	if err := r.PlaceBet("bob", 100); err != nil {
		t.Fatalf("placebet bob: %v", err)
	}

	stakes := r.SeatStakes()
	if stakes["alice"] != 50 || stakes["bob"] != 100 {
		t.Fatalf("stakes: %+v", stakes)
	}

	if err := r.Settle(); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if r.State != gamev1.GameState_GAME_STATE_SETTLING {
		t.Fatalf("want SETTLING, got %v", r.State)
	}

	// Settle 後再 Settle 應拒絕
	if err := r.Settle(); err != ErrWrongState {
		t.Fatalf("want ErrWrongState, got %v", err)
	}
}

func TestRoom_PlaceBet_RoomClosedRejected(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	m.Remove(r.ID)

	if err := r.PlaceBet("alice", 50); err != ErrWrongState {
		t.Fatalf("want ErrWrongState after close, got %v", err)
	}
}

func TestRoom_RefundBet(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	_ = r.PlaceBet("alice", 50)
	if err := r.RefundBet("alice", 30); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if s, _ := r.SeatStake("alice"); s != 20 {
		t.Fatalf("stake after refund: %d; want 20", s)
	}
}

func TestRoom_WithGameState(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	if err := r.WithGameState(func(gs map[string]any) error {
		gs["active_seat"] = 1
		gs["round"] = "preflop"
		return nil
	}); err != nil {
		t.Fatalf("withstate: %v", err)
	}
	if v, ok := r.ReadGameState("active_seat"); !ok || v.(int) != 1 {
		t.Fatalf("read active_seat: %v ok=%v", v, ok)
	}
}

func TestRoom_SubscribeReceivesSnapshot(t *testing.T) {
	m := newTestManager()
	r, _ := m.Create("alice", "ddz", 100, 3)
	ch, cancel, err := r.Subscribe("alice")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	select {
	case ev := <-ch:
		if ev.GetSnapshot() == nil {
			t.Fatalf("first event not snapshot: %+v", ev)
		}
	default:
		t.Fatal("no initial snapshot delivered")
	}
}
