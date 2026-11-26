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
	"github.com/PedroCarassale/nimbus/compute-plane/internal/firecracker"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/handlers"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/isolate"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/metrics"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/objects"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[compute-plane] Iniciando servicio...")
	stopTrace := ntrace.Setup(context.Background())

	port := getEnv("PORT", "8080")
	redisAddr := getEnv("REDIS_URL", "redis:6379")

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

	objCfg := objects.LoadConfig()
	mc, err := objects.New(ctx, objCfg)
	if err != nil {
		log.Fatalf("[compute-plane] Error conectando a S3: %v", err)
	}
	endpoint := objCfg.Endpoint
	if endpoint == "" {
		endpoint = "aws:" + objCfg.Region
	}
	log.Printf("[compute-plane] S3 bucket=%s endpoint=%s pathStyle=%v", objCfg.Bucket, endpoint, objCfg.ForcePathStyle)

	concCfg := concurrency.LoadConfig()
	log.Printf("[compute-plane] Concurrencia por función=%d cola=%d espera=%s containers=%d lease=%s",
		concCfg.MaxConcurrency, concCfg.QueueMax, concCfg.MaxWait, concCfg.GlobalMax, concCfg.Lease)
	fnDefaults := fnconfig.LoadDefaults()
	log.Printf("[compute-plane] Config por función: timeout=%ds memory=%dMB handler=%s cpus=%s",
		fnDefaults.TimeoutSec, fnDefaults.MemoryMB, fnDefaults.Handler, fnDefaults.CPUsLabel())
	concurrencyMgr := concurrency.NewManager(rdb, concCfg)
	concurrencyMgr.StartSweeper()
	iso, isoErr := isolate.Parse(os.Getenv("NIMBUS_ISOLATION"))
	if isoErr != nil {
		log.Fatalf("[compute-plane] %v", isoErr)
	}
	log.Printf("[compute-plane] Aislamiento: %s", iso)
	if iso == isolate.Firecracker {
		firecracker.CleanupLeaked()
	} else if err := warm.CleanupOrphans(ctx); err != nil {
		log.Printf("[compute-plane] No se pudieron limpiar containers warm anteriores: %v", err)
	}
	exec := executor.New(mc, rdb)
	exec.StartBackground()
	handler := handlers.NewWithDefaults(exec, concurrencyMgr, fnDefaults)

	asyncCfg := async.LoadConfig()
	backendName, err := async.ParseBackend(os.Getenv("NIMBUS_ASYNC_BACKEND"))
	if err != nil {
		log.Fatalf("[compute-plane] %v", err)
	}
	var (
		queuer  async.Queuer
		workers async.Backend
	)
	switch backendName {
	case "sqs":
		settings := async.LoadSQSSettings()
		sqsQueue, sqsErr := async.NewSQS(ctx, rdb, asyncCfg, settings)
		if sqsErr != nil {
			log.Fatalf("[compute-plane] Error conectando a SQS: %v", sqsErr)
		}
		queuer = sqsQueue
		workers = sqsQueue
		where := settings.Endpoint
		if where == "" {
			where = "aws:" + settings.Region
		}
		log.Printf("[compute-plane] Async backend=sqs endpoint=%s queue=%s dlq=%s", where, sqsQueue.JobsURL(), sqsQueue.DLQURL())
	default:
		redisQueue := async.New(rdb, asyncCfg)
		queuer = redisQueue
		workers = redisQueue
		log.Printf("[compute-plane] Async backend=redis")
	}
	if err := workers.EnsureGroup(ctx); err != nil {
		log.Printf("[compute-plane] Cola async no disponible todavía: %v", err)
	}
	handler.SetQueue(queuer)
	metrics.SetSources(metrics.Sources{
		QueueDepth: func(ctx context.Context) map[string]float64 {
			depths, err := concurrencyMgr.QueueDepths(ctx)
			if err != nil {
				return nil
			}
			out := make(map[string]float64, len(depths))
			for id, n := range depths {
				out[id] = float64(n)
			}
			return out
		},
		DLQEntries: func(ctx context.Context) float64 {
			n, err := queuer.DLQLen(ctx)
			if err != nil {
				return 0
			}
			return float64(n)
		},
		ContainersInUse: func(ctx context.Context) float64 {
			n, err := concurrencyMgr.GlobalInUse(ctx)
			if err != nil {
				return 0
			}
			return float64(n)
		},
		ContainersLimit: float64(concCfg.GlobalMax),
		WarmPool:        exec.WarmPool,
	})
	var stopAsync func()
	if asyncCfg.Enabled {
		log.Printf("[compute-plane] Async: intentos=%d backoff=%s..%s jitter=%s claim=%s lock=%s workers=%d",
			asyncCfg.MaxAttempts, asyncCfg.BackoffBase, asyncCfg.BackoffMax, asyncCfg.BackoffJitter,
			asyncCfg.ClaimIdle, asyncCfg.LockTTL, asyncCfg.Workers)
		stopAsync = async.StartWorkers(workers, handler.RunJob, asyncCfg.Workers)
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
	r.Handle("/metrics", promhttp.Handler())
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
	_ = stopTrace(context.Background())
	exec.Shutdown(context.Background())

	log.Println("[compute-plane] Servidor detenido")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
