// Package discovery 向 Consul 註冊遊戲服務並設定 gRPC health check。
// 僅供 pkg-game-framework 內部使用。
package discovery

import (
	"fmt"

	consulapi "github.com/hashicorp/consul/api"
)

// Register 向 Consul 註冊服務並設定 gRPC health check。
// 每 5s 檢查一次，失敗 30s 後自動移除。
func Register(addr, serviceName, instanceID, host string, port int, tags []string) (deregister func() error, err error) {
	cfg := consulapi.DefaultConfig()
	cfg.Address = addr
	client, err := consulapi.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("consul client: %w", err)
	}
	reg := &consulapi.AgentServiceRegistration{
		ID:      fmt.Sprintf("%s-%s", serviceName, instanceID),
		Name:    serviceName,
		Tags:    tags,
		Address: host,
		Port:    port,
		Meta: map[string]string{
			"instance_id": instanceID,
			"grpc_port":   fmt.Sprintf("%d", port),
		},
		Check: &consulapi.AgentServiceCheck{
			GRPC:                           fmt.Sprintf("%s:%d", host, port),
			Interval:                       "5s",
			Timeout:                        "2s",
			DeregisterCriticalServiceAfter: "30s",
		},
	}
	if err := client.Agent().ServiceRegister(reg); err != nil {
		return nil, fmt.Errorf("consul register: %w", err)
	}
	return func() error { return client.Agent().ServiceDeregister(reg.ID) }, nil
}
