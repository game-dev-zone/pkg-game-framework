package framework

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	gamev1 "github.com/club8/pkg-proto/gen/go/club/game/v1"
	recordv1 "github.com/club8/pkg-proto/gen/go/club/record/v1"
	txv1 "github.com/club8/pkg-proto/gen/go/club/tx/v1"
	"github.com/club8/pkg-game-framework/internal/discovery"
	"github.com/club8/pkg-game-framework/internal/grpcserver"
	"github.com/club8/pkg-game-framework/internal/recordclient"
	"github.com/club8/pkg-game-framework/internal/session"
	"github.com/club8/pkg-game-framework/internal/txclient"
	"github.com/club8/pkg-game-framework/room"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Run 啟動遊戲服務的完整生命週期：連 Tx / 連 Record / 連 ETCD / 起 gRPC /
// 註冊 Consul /（可選）啟動 tick。阻塞至收到 SIGINT/SIGTERM 才返回。
//
// 外部遊戲開發者的 main 通常如下：
//
//	func main() {
//	    ctx, cancel := context.WithCancel(context.Background())
//	    defer cancel()
//	    cfg := framework.LoadFromEnv(framework.Config{})
//	    if err := framework.Run(ctx, cfg, &logic.Niuniu{}); err != nil {
//	        log.Fatal(err)
//	    }
//	}
func Run(parent context.Context, cfg Config, logic GameLogic) error {
	meta := logic.Meta()
	if meta.GameID == "" {
		return fmt.Errorf("GameLogic.Meta().GameID is required")
	}

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "game-"+meta.GameID).
		Logger()

	cfg = LoadFromEnv(cfg)
	if cfg.InstanceID == "" {
		cfg.InstanceID = uuid.NewString()
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "game-" + meta.GameID
	}
	logger = logger.With().Str("instance_id", cfg.InstanceID).Logger()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Tx client（必需）
	txc, err := txclient.Dial(cfg.ConsulAddr, cfg.TxServiceName)
	if err != nil {
		return fmt.Errorf("tx dial: %w", err)
	}
	defer txc.Close()

	// Record client（可選；dev 環境沒起 record 時允許繼續）
	var recClient recordv1.RecordServiceClient
	if cfg.RecordSvcName != "" {
		rc, err := recordclient.Dial(cfg.ConsulAddr, cfg.RecordSvcName)
		if err != nil {
			logger.Warn().Err(err).Str("svc", cfg.RecordSvcName).
				Msg("record dial failed; continuing without record writes")
		} else {
			defer rc.Close()
			recClient = rc.Raw()
		}
	}

	// ETCD session
	sess, err := session.Dial(cfg.EtcdEndpoints, meta.GameID, session.Entry{
		InstanceID: cfg.InstanceID,
		GRPCAddr:   fmt.Sprintf("%s:%d", cfg.AdvertiseHost, cfg.GRPCPort),
	}, logger)
	if err != nil {
		return fmt.Errorf("etcd dial: %w", err)
	}
	defer sess.Close()
	if err := sess.Start(ctx); err != nil {
		return fmt.Errorf("etcd lease: %w", err)
	}

	mgr := room.NewManager(logger)
	defer mgr.CloseAll()

	adapter := newLogicAdapter(logic)
	newCtx := func(parent context.Context, traceID string) grpcserver.FrameworkContext {
		return &fctx{Context: parent, traceID: traceID, log: logger, tx: txc.Raw(), record: recClient}
	}

	svc := grpcserver.New(mgr, sess, txc, recClient, adapter, newCtx, cfg.RecordTimeout, logger)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	grpcServer := grpc.NewServer()
	gamev1.RegisterGameServiceServer(grpcServer, svc)

	healthSvc := health.NewServer()
	healthSvc.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSvc.SetServingStatus("club.game.v1.GameService", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSvc)
	reflection.Register(grpcServer)

	tags := append([]string{"v1", "grpc", "room", meta.GameID, cfg.Env}, cfg.ExtraTags...)
	deregister, err := discovery.Register(
		cfg.ConsulAddr, cfg.ServiceName, cfg.InstanceID,
		cfg.AdvertiseHost, cfg.GRPCPort, tags,
	)
	if err != nil {
		logger.Warn().Err(err).Msg("consul register failed; continuing")
	}

	// Tick goroutine（可選）
	if meta.TickInterval > 0 {
		go runTicks(ctx, meta.TickInterval, mgr, logic, newCtx, logger)
	}

	go func() {
		logger.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Fatal().Err(err).Msg("grpc serve")
		}
	}()

	// 等待 shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-sig:
		logger.Info().Msg("shutdown signal received")
	}

	if deregister != nil {
		_ = deregister()
	}
	healthSvc.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	grpcServer.GracefulStop()
	logger.Info().Msg("bye")
	return nil
}

func runTicks(
	ctx context.Context,
	interval time.Duration,
	mgr *room.Manager,
	logic GameLogic,
	newCtx func(context.Context, string) grpcserver.FrameworkContext,
	log zerolog.Logger,
) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mgr.ForEach(func(r *room.Room) {
				tctx := newCtx(ctx, uuid.NewString())
				if err := logic.OnTick(tctx, r); err != nil {
					log.Warn().Err(err).Str("room_id", r.ID).Msg("tick failed; closing room")
					mgr.Remove(r.ID)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// fctx：Context interface 的實作
// ---------------------------------------------------------------------------

type fctx struct {
	context.Context
	traceID string
	log     zerolog.Logger
	tx      txv1.TxServiceClient
	record  recordv1.RecordServiceClient
}

func (c *fctx) TraceID() string                     { return c.traceID }
func (c *fctx) Logger() zerolog.Logger              { return c.log.With().Str("trace_id", c.traceID).Logger() }
func (c *fctx) Tx() txv1.TxServiceClient            { return c.tx }
func (c *fctx) Record() recordv1.RecordServiceClient { return c.record }

// ---------------------------------------------------------------------------
// logicAdapter：把 framework.GameLogic 轉成 grpcserver.LogicAdapter
// ---------------------------------------------------------------------------

type logicAdapter struct {
	meta  GameMeta
	logic GameLogic
}

func newLogicAdapter(l GameLogic) *logicAdapter {
	return &logicAdapter{meta: l.Meta(), logic: l}
}

func (a *logicAdapter) GameID() string     { return a.meta.GameID }
func (a *logicAdapter) DefaultSeats() int32 { return a.meta.DefaultSeats }

func (a *logicAdapter) OnCreateRoom(ctx grpcserver.FrameworkContext, req *gamev1.CreateRoomRequest, r *room.Room) error {
	return a.logic.OnCreateRoom(ctx.(*fctx), req, r)
}

func (a *logicAdapter) OnEnterRoom(ctx grpcserver.FrameworkContext, req *gamev1.EnterRoomRequest, r *room.Room) error {
	return a.logic.OnEnterRoom(ctx.(*fctx), req, r)
}

func (a *logicAdapter) OnPlaceBet(ctx grpcserver.FrameworkContext, req *gamev1.PlaceBetRequest, r *room.Room) error {
	return a.logic.OnPlaceBet(ctx.(*fctx), req, r)
}

func (a *logicAdapter) OnSettle(ctx grpcserver.FrameworkContext, req *gamev1.SettleRequest, r *room.Room) ([]*gamev1.Payout, error) {
	return a.logic.OnSettle(ctx.(*fctx), req, r)
}
