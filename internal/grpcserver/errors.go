package grpcserver

import (
	commonv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/common/v1"
	gamev1 "github.com/game-dev-zone/pkg-proto/gen/go/club/game/v1"
	"github.com/game-dev-zone/pkg-game-framework/room"
)

func codeFromRoomErr(err error) commonv1.ErrorCode {
	switch err {
	case room.ErrRoomFull:
		return commonv1.ErrorCode_ERROR_CODE_ROOM_FULL
	case room.ErrAlreadyJoin:
		return commonv1.ErrorCode_ERROR_CODE_ALREADY_IN_ROOM
	case room.ErrNotInRoom:
		return commonv1.ErrorCode_ERROR_CODE_ROOM_NOT_FOUND
	case room.ErrNotOwner:
		return commonv1.ErrorCode_ERROR_CODE_NOT_ROOM_OWNER
	case room.ErrWrongState:
		return commonv1.ErrorCode_ERROR_CODE_GAME_STATE_INVALID
	default:
		return commonv1.ErrorCode_ERROR_CODE_INTERNAL
	}
}

func okAck() *commonv1.Ack {
	return &commonv1.Ack{Code: commonv1.ErrorCode_ERROR_CODE_OK}
}

// errResp 泛型建構業務錯誤 response（*.Ack 欄位）。
// T 必須是 gamev1 中有 Ack 的 response 型別。
func errResp[T any](code commonv1.ErrorCode, msg string) *T {
	ack := &commonv1.Ack{Code: code, Message: msg}
	var zero T
	switch any(&zero).(type) {
	case *gamev1.CreateRoomResponse:
		return any(&gamev1.CreateRoomResponse{Ack: ack}).(*T)
	case *gamev1.EnterRoomResponse:
		return any(&gamev1.EnterRoomResponse{Ack: ack}).(*T)
	case *gamev1.LeaveRoomResponse:
		return any(&gamev1.LeaveRoomResponse{Ack: ack}).(*T)
	case *gamev1.PlaceBetResponse:
		return any(&gamev1.PlaceBetResponse{Ack: ack}).(*T)
	case *gamev1.SettleResponse:
		return any(&gamev1.SettleResponse{Ack: ack}).(*T)
	}
	panic("unsupported response type")
}
