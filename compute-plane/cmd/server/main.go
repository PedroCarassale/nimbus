package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/handlers"
	minioClient "github.com/PedroCarassale/nimbus/compute-plane/internal/minio"
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

	concurrencyMgr := concurrency.NewManager(rdb)
	exec := executor.New(mc, rdb)
	handler := handlers.New(exec, concurrencyMgr)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(120 * time.Second))

	r.Get("/health", handler.Health)
	r.Post("/executions", handler.StartExecution)
	r.Post("/executions/{id}/cancel", handler.CancelExecution)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
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

	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("[compute-plane] Error en shutdown: %v", err)
	}

	log.Println("[compute-plane] Servidor detenido")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
