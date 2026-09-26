package concurrency

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Manager maneja locks de concurrencia por función usando Redis.
// Implementa un lock distribuido simple con TTL para evitar deadlocks.
//
// Esquema de keys Redis:
//   - nimbus:lock:fn:{functionId} — lock exclusivo por función (SET NX EX)
//   - nimbus:exec:{executionId} — metadata de ejecución activa (HSET)
//
// El lock tiene un TTL de 2x el timeout máximo permitido para asegurar
// que se libere automáticamente si el proceso muere.
type Manager struct {
	rdb       *redis.Client
	lockTTL   time.Duration
	maxWait   time.Duration
	pollDelay time.Duration
}

func NewManager(rdb *redis.Client) *Manager {
	return &Manager{
		rdb:       rdb,
		lockTTL:   5 * time.Minute,
		maxWait:   30 * time.Second,
		pollDelay: 100 * time.Millisecond,
	}
}

func (m *Manager) lockKey(functionID string) string {
	return fmt.Sprintf("nimbus:lock:fn:%s", functionID)
}

func (m *Manager) execKey(executionID string) string {
	return fmt.Sprintf("nimbus:exec:%s", executionID)
}

func (m *Manager) AcquireLock(ctx context.Context, functionID, executionID string) (bool, error) {
	key := m.lockKey(functionID)
	deadline := time.Now().Add(m.maxWait)

	for {
		if time.Now().After(deadline) {
			return false, nil
		}

		ok, err := m.rdb.SetNX(ctx, key, executionID, m.lockTTL).Result()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			log.Printf("[concurrency] Warning: error Redis SetNX: %v", err)
			time.Sleep(m.pollDelay)
			continue
		}

		if ok {
			log.Printf("[concurrency] Lock adquirido para función %s (exec: %s)", functionID, executionID)
			execKey := m.execKey(executionID)
			m.rdb.HSet(ctx, execKey, map[string]interface{}{
				"functionId": functionID,
				"status":     "running",
				"startedAt":  time.Now().Unix(),
			})
			m.rdb.Expire(ctx, execKey, m.lockTTL)
			return true, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(m.pollDelay):
		}
	}
}

func (m *Manager) ReleaseLock(ctx context.Context, functionID, executionID string) error {
	key := m.lockKey(functionID)

	current, err := m.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error obteniendo lock: %w", err)
	}

	if current != executionID {
		log.Printf("[concurrency] Warning: lock para %s pertenece a %s, no a %s", functionID, current, executionID)
		return nil
	}

	_, err = m.rdb.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("error liberando lock: %w", err)
	}

	execKey := m.execKey(executionID)
	m.rdb.Del(ctx, execKey)

	log.Printf("[concurrency] Lock liberado para función %s (exec: %s)", functionID, executionID)
	return nil
}

func (m *Manager) MarkForCancellation(ctx context.Context, executionID string) error {
	execKey := m.execKey(executionID)

	exists, err := m.rdb.Exists(ctx, execKey).Result()
	if err != nil {
		return fmt.Errorf("error verificando ejecución: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("ejecución %s no encontrada", executionID)
	}

	_, err = m.rdb.HSet(ctx, execKey, "cancelled", true).Result()
	if err != nil {
		return fmt.Errorf("error marcando cancelación: %w", err)
	}

	log.Printf("[concurrency] Ejecución %s marcada para cancelación", executionID)
	return nil
}

func (m *Manager) IsCancelled(ctx context.Context, executionID string) bool {
	execKey := m.execKey(executionID)
	cancelled, err := m.rdb.HGet(ctx, execKey, "cancelled").Result()
	if err != nil {
		return false
	}
	return cancelled == "1" || cancelled == "true"
}
