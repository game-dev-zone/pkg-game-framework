// Package recordclient 封裝 RecordService 的撥號與呼叫包裝。
// 僅供 pkg-game-framework 內部使用。
package recordclient

import (
	_ "github.com/mbobakov/grpc-consul-resolver"

	recordv1 "github.com/club8/pkg-proto/gen/go/club/record/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn *grpc.ClientConn
	c    recordv1.RecordServiceClient
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
	return &Client{conn: conn, c: recordv1.NewRecordServiceClient(conn)}, nil
}

func (c *Client) Raw() recordv1.RecordServiceClient { return c.c }
func (c *Client) Close() error                      { return c.conn.Close() }
