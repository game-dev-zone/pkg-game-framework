// Package txclient 封裝 Tx Service 的撥號與 idempotency key 產生。
// 僅供 pkg-game-framework 內部使用；GameLogic 需要呼叫 Tx 時
// 應透過 framework.Context.Tx() escape hatch（預設 lifecycle 會處理 99% 場景）。
package txclient

import (
	_ "github.com/mbobakov/grpc-consul-resolver" // register consul resolver scheme

	txv1 "github.com/game-dev-zone/pkg-proto/gen/go/club/tx/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn *grpc.ClientConn
	c    txv1.TxServiceClient
}

func Dial(consulAddr, serviceName string) (*Client, error) {
	conn, err := grpc.NewClient(
		"consul://"+consulAddr+"/"+serviceName+"?wait=14s&healthy=true&tag=v1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, c: txv1.NewTxServiceClient(conn)}, nil
}

func (c *Client) Raw() txv1.TxServiceClient { return c.c }
func (c *Client) Close() error              { return c.conn.Close() }
