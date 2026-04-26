package framework

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 由 framework.Run 使用；欄位零值代表「由 LoadFromEnv 覆蓋」，
// 允許遊戲作者只覆寫想自訂的欄位，其餘走環境變數。
type Config struct {
	GRPCPort      int
	ConsulAddr    string
	EtcdEndpoints []string
	TxServiceName string
	RecordSvcName string
	ServiceName   string // Consul 註冊名；預設 "game-<game_id>"
	InstanceID    string // 空字串 → Run 補 UUID
	AdvertiseHost string
	Env           string
	ExtraTags     []string

	// RecordTimeout 控制框架寫 record 的上限；超時不影響 Settle 回應。
	RecordTimeout time.Duration

	// ReportServiceURL：若非空且 GameLogic 實作 SettlementMeta，框架會在
	// Settle 完成後 POST /internal/reports/ingest 產 match_settlement。
	ReportServiceURL string

	// CardServiceURL：若非空且 SettlementMeta.RakeCard > 0，框架會在 Settle
	// 完成後 POST /internal/cards/consume 從俱樂部扣抽水房卡。
	CardServiceURL string
}

// LoadFromEnv 以環境變數覆蓋 cfg 中的零值欄位並回傳填充後的 Config。
// 呼叫端通常：cfg := framework.LoadFromEnv(framework.Config{})
func LoadFromEnv(cfg Config) Config {
	if cfg.GRPCPort == 0 {
		cfg.GRPCPort = envInt("GRPC_PORT", 9201)
	}
	if cfg.ConsulAddr == "" {
		cfg.ConsulAddr = envStr("CONSUL_ADDR", "127.0.0.1:8500")
	}
	if len(cfg.EtcdEndpoints) == 0 {
		cfg.EtcdEndpoints = strings.Split(envStr("ETCD_ENDPOINTS", "127.0.0.1:2379"), ",")
	}
	if cfg.TxServiceName == "" {
		cfg.TxServiceName = envStr("TX_SERVICE_NAME", "tx")
	}
	if cfg.RecordSvcName == "" {
		cfg.RecordSvcName = envStr("RECORD_SERVICE_NAME", "record")
	}
	if cfg.AdvertiseHost == "" {
		cfg.AdvertiseHost = envStr("ADVERTISE_HOST", "127.0.0.1")
	}
	if cfg.Env == "" {
		cfg.Env = envStr("ENV", "dev")
	}
	if cfg.RecordTimeout == 0 {
		cfg.RecordTimeout = 2 * time.Second
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = envStr("INSTANCE_ID", "")
	}
	if cfg.ReportServiceURL == "" {
		cfg.ReportServiceURL = envStr("REPORT_SERVICE_URL", "")
	}
	if cfg.CardServiceURL == "" {
		cfg.CardServiceURL = envStr("CARD_SERVICE_URL", "")
	}
	return cfg
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
