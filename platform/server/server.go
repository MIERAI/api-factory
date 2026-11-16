// Package server 是运行时模板的启动部分。所有生成出来的服务共用,AI 不可修改。
//
// 提供: 配置读取 / DB 连接池 / 拦截器链(日志→错误清洗→超时→校验) / healthz / 优雅停机
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"apifactory/platform/apierr"
	"apifactory/platform/paging"
)

// Service 是一个已挂载的 Connect 服务。
type Service struct {
	Path    string
	Handler http.Handler
}

// Deps 是平台提供给 handler 的依赖。
type Deps struct {
	Pool  *pgxpool.Pool
	Pager *paging.Pager
}

// Builder 由生成的 wiring 代码实现,负责把 handler 挂到服务上。
type Builder func(deps Deps, opts ...connect.HandlerOption) []Service

type config struct {
	port           string
	databaseURL    string
	requestTimeout time.Duration
}

func loadConfig() (config, error) {
	c := config{
		port:           envOr("PORT", "8080"),
		databaseURL:    os.Getenv("DATABASE_URL"),
		requestTimeout: 3 * time.Second,
	}
	if c.databaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if v := os.Getenv("REQUEST_TIMEOUT_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("REQUEST_TIMEOUT_MS: %w", err)
		}
		c.requestTimeout = time.Duration(ms) * time.Millisecond
	}
	return c, nil
}

// Run 启动服务,直到收到 SIGINT/SIGTERM。
func Run(build Builder) {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, build); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, build Builder) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("db config: %w", err)
	}
	// 匿名预编译语句: Postgres 每次都用实际参数值做执行计划。
	// 生成的查询大量使用 "(@x IS NULL OR col = @x)" 形式的可选条件和 keyset 游标,
	// 命名预编译语句在执行若干次后可能切换成通用计划,游标就不再是索引条件(深页退化成 OFFSET)。
	// 一次往返,PgBouncer 事务模式下也可用。
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	opts := connect.WithInterceptors(
		newLoggingInterceptor(log),
		apierr.NewSanitizeInterceptor(log),
		newTimeoutInterceptor(cfg.requestTimeout),
		validate.NewInterceptor(),
	)

	mux := http.NewServeMux()
	if os.Getenv("PAGE_TOKEN_KEY") == "" {
		log.Warn("PAGE_TOKEN_KEY not set: using a random key (page tokens break across restarts/instances)")
	}
	deps := Deps{Pool: pool, Pager: paging.New([]byte(os.Getenv("PAGE_TOKEN_KEY")))}
	for _, s := range build(deps, opts) {
		mux.Handle(s.Path, s.Handler)
		log.Info("service mounted", "path", s.Path)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC 需要 h2c
	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "port", cfg.port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func newLoggingInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			res, err := next(ctx, req)
			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			log.InfoContext(ctx, "rpc",
				"procedure", req.Spec().Procedure,
				"code", code,
				"duration_ms", time.Since(start).Milliseconds(),
			)
			return res, err
		}
	}
}

func newTimeoutInterceptor(d time.Duration) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
