package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/registry"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/scheduler"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[scheduler] Iniciando servicio...")
	stopTrace := ntrace.Setup(context.Background())

	port := env("PORT", "8080")
	rdb := redis.NewClient(&redis.Options{
		Addr:     env("REDIS_URL", "redis:6379"),
		Password: env("REDIS_PASSWORD", ""),
		DB:       0,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("[scheduler] Warning: Redis no disponible aún: %v", err)
	} else {
		log.Println("[scheduler] Conectado a Redis")
	}

	ttl := registry.DefaultTTL
	if raw := os.Getenv("NIMBUS_HEARTBEAT_TTL_MS"); raw != "" {
		if n, err := time.ParseDuration(raw + "ms"); err == nil && n > 0 {
			ttl = n
		}
	}
	reg := registry.New(rdb, ttl)
	srvHandler := scheduler.New(reg, fnconfig.LoadDefaults(), nil)
	budget := scheduler.RequestBudget()
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      srvHandler.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: budget,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("[scheduler] HTTP en :%s (TTL de workers %s)", port, ttl)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[scheduler] Error del servidor: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[scheduler] Apagando...")
	ctxShutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("[scheduler] shutdown: %v", err)
	}
	_ = stopTrace(context.Background())
	log.Println("[scheduler] Detenido")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
