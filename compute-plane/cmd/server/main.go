package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/async"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/handlers"
	minioClient "github.com/PedroCarassale/nimbus/compute-plane/internal/minio"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[compute-plane] Iniciando servicio...")

	port := getEnv("PORT", "8080")
	redisAddr := getEnv("REDIS_URL", "redis:6379")
	minioEndpoint := getEnv("MINIO_ENDPOINT", "minio:9000")
	minioAccessKey := getEnv("MINIO_ACCESS_KEY", "minioadmin")
	minioSecretKey := getEnv("MINIO_SECRET_KEY", "minioadmin")
	minioBucket := getEnv("MINIO_BUCKET", "nimbus-artifacts")
	minioUseSSL := getEnv("MINIO_USE_SSL", "false") == "true"

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: getEnv("REDIS_PASSWORD", ""),
		DB:       0,
	})

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("[compute-plane] Warning: Redis no disponible aún: %v", err)
	} else {
		log.Println("[compute-plane] Conectado a Redis")
	}

	mc, err := minioClient.New(minioEndpoint, minioAccessKey, minioSecretKey, minioBucket, minioUseSSL)
	if err != nil {
		log.Fatalf("[compute-plane] Error conectando a MinIO: %v", err)
	}
	log.Println("[compute-plane] Conectado a MinIO")

	concCfg := concurrency.LoadConfig()
	log.Printf("[compute-plane] Concurrencia por función=%d cola=%d espera=%s containers=%d lease=%s",
		concCfg.MaxConcurrency, concCfg.QueueMax, concCfg.MaxWait, concCfg.GlobalMax, concCfg.Lease)
	fnDefaults := fnconfig.LoadDefaults()
	log.Printf("[compute-plane] Config por función: timeout=%ds memory=%dMB handler=%s cpus=%s",
		fnDefaults.TimeoutSec, fnDefaults.MemoryMB, fnDefaults.Handler, fnDefaults.CPUsLabel())
	concurrencyMgr := concurrency.NewManager(rdb, concCfg)
	if err := warm.CleanupOrphans(ctx); err != nil {
		log.Printf("[compute-plane] No se pudieron limpiar containers warm anteriores: %v", err)
	}
	exec := executor.New(mc, rdb)
	exec.StartBackground()
	handler := handlers.NewWithDefaults(exec, concurrencyMgr, fnDefaults)

	asyncCfg := async.LoadConfig()
	asyncQueue := async.New(rdb, asyncCfg)
	if err := asyncQueue.EnsureGroup(ctx); err != nil {
		log.Printf("[compute-plane] Cola async no disponible todavía: %v", err)
	}
	handler.SetQueue(asyncQueue)
	var stopAsync func()
	if asyncCfg.Enabled {
		log.Printf("[compute-plane] Async: intentos=%d backoff=%s..%s jitter=%s claim=%s lock=%s workers=%d",
			asyncCfg.MaxAttempts, asyncCfg.BackoffBase, asyncCfg.BackoffMax, asyncCfg.BackoffJitter,
			asyncCfg.ClaimIdle, asyncCfg.LockTTL, asyncCfg.Workers)
		stopAsync = async.StartWorkers(asyncQueue, handler.RunJob, asyncCfg.Workers)
	} else {
		log.Printf("[compute-plane] Workers async apagados (NIMBUS_ASYNC_ENABLED)")
	}

	// The HTTP deadline has to outlive the longest accepted execution plus
	// the queue wait. Chi's timeout cancels the request context, which is
	// what kills docker run.
	requestBudget := time.Duration(fnconfig.TimeoutMax)*time.Second + concCfg.MaxWait + 45*time.Second

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestBudget))

	r.Get("/health", handler.Health)
	r.Head("/health", handler.Health)
	r.Post("/executions", handler.StartExecution)
	r.Post("/executions/{id}/cancel", handler.CancelExecution)
	r.Post("/async/jobs", handler.EnqueueAsync)
	r.Get("/dlq", handler.ListDLQ)
	r.Post("/dlq/{id}/redrive", handler.RedriveDLQ)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: requestBudget,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("[compute-plane] Servidor HTTP escuchando en :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[compute-plane] Error del servidor: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[compute-plane] Apagando servidor...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if stopAsync != nil {
		stopAsync()
	}
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("[compute-plane] Error en shutdown: %v", err)
	}
	exec.Shutdown(context.Background())

	log.Println("[compute-plane] Servidor detenido")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
