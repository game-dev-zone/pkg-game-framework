// Package session 以 etcd 寫入 room_id → instance_id 的映射（帶 lease），
// 讓 gateway 在既有房間上能把流量固定導向同一 game 實例。
//
// 僅供 pkg-game-framework 內部使用；GameLogic 不應直接碰 ETCD。
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const leaseTTL = 30 // seconds

type Entry struct {
	InstanceID string `json:"instance_id"`
	GRPCAddr   string `json:"grpc_addr"`
}

type Store struct {
	client *clientv3.Client
	gameID string
	self   Entry
	log    zerolog.Logger

	leaseID clientv3.LeaseID
	cancel  context.CancelFunc
}

func Dial(endpoints []string, gameID string, self Entry, log zerolog.Logger) (*Store, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd dial: %w", err)
	}
	return &Store{
		client: cli,
		gameID: gameID,
		self:   self,
		log:    log.With().Str("component", "session.etcd").Logger(),
	}, nil
}

// Start 啟動 lease keepalive。ctx 取消時自動撤銷 lease。
func (s *Store) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel

	lease, err := s.client.Grant(ctx, leaseTTL)
	if err != nil {
		return fmt.Errorf("lease grant: %w", err)
	}
	s.leaseID = lease.ID

	ch, err := s.client.KeepAlive(ctx, s.leaseID)
	if err != nil {
		return fmt.Errorf("lease keepalive: %w", err)
	}
	go func() {
		for range ch {
			// drain; 斷線由 etcd client 自動處理重連
		}
		s.log.Warn().Msg("etcd keepalive channel closed")
	}()
	return nil
}

// Bind 寫入 room → self 的映射。
func (s *Store) Bind(ctx context.Context, roomID string) error {
	key := fmt.Sprintf("/rooms/%s/%s", s.gameID, roomID)
	val, _ := json.Marshal(s.self)
	_, err := s.client.Put(ctx, key, string(val), clientv3.WithLease(s.leaseID))
	return err
}

// Unbind 移除房間映射（結束房時呼叫）。
func (s *Store) Unbind(ctx context.Context, roomID string) error {
	key := fmt.Sprintf("/rooms/%s/%s", s.gameID, roomID)
	_, err := s.client.Delete(ctx, key)
	return err
}

func (s *Store) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.client.Close()
}
