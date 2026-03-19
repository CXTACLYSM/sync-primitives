# Модуль 010 — Production Assembly & Stress Test

## Контекст

Девять модулей построили все компоненты: counting semaphore (001), context-aware acquire (002), weighted semaphore (003), SafeMap с RWMutex (004), token bucket rate limiter (005), connection pool core (006), sync.Pool для буферов (007), bulkhead isolation (008), singleflight дедупликация (009). Каждый компонент работает и протестирован отдельно.

Этот модуль — финальная интеграция. Все компоненты собираются в один Connection Pool Manager. Stress test с сотнями горутин проверяет корректность под нагрузкой. Race detector подтверждает отсутствие data races. pprof профилирует горячие пути. Метрики экспортируются для мониторинга.

Результат — пакет `connpool` готовый к использованию в проектах 4–10.

## Что нужно реализовать

**Финальный `PoolManager`** — фасад объединяющий все компоненты:

```go
type PoolManager struct {
    pool      *connpool.Pool
    bulkhead  *bulkhead.Bulkhead
    limiter   *ratelimit.RateLimiter
    dedup     *dedup.Group
    buffers   *bufpool.BufferPool
    metrics   *PoolMetrics
    logger    *slog.Logger
    closed    atomic.Bool
}
```

**Конструктор:**

```go
func NewPoolManager(factory connpool.Factory, opts ...ManagerOption) *PoolManager
```

**ManagerOptions:**

```go
WithMaxConns(n int)
WithMaxIdle(n int)
WithMaxLifetime(d time.Duration)
WithMaxIdleTime(d time.Duration)
WithRateLimit(rate float64, burst int)
WithPartition(name string, maxConns int)
WithFallbackPartition(name string)
WithHealthCheckInterval(d time.Duration)
WithBufferPoolSize(size int)
WithLogger(logger *slog.Logger)
WithMetricsEnabled(enabled bool)
```

**Методы PoolManager:**

- `Acquire(ctx context.Context, partition string) (*ManagedConn, error)` — полный pipeline: rate limit → bulkhead → pool acquire → buffer get. Возвращает `ManagedConn` — обёртка с buffer, partition info, metrics tracking.

- `Release(conn *ManagedConn)` — обратный pipeline: buffer put → pool release → bulkhead release. Записывает метрики duration.

- `DedupDo(key string, fn func(*ManagedConn) (any, error), partition string) (any, error)` — singleflight-обёрнутая операция: acquire conn, execute fn, release conn. Дедупликация по key.

- `HealthCheck(ctx context.Context) error` — параллельный health check через errgroup.

- `Stats() ManagerStats` — агрегированные метрики всех компонентов.

- `Close() error` — graceful shutdown всех компонентов.

**Тип `ManagedConn`:**

```go
type ManagedConn struct {
    conn      *connpool.Conn
    buf       *bytes.Buffer    // из BufferPool
    partition string
    manager   *PoolManager
    startTime time.Time        // для метрик duration
    released  atomic.Bool
}
```

**Тип `PoolMetrics`:**

```go
type PoolMetrics struct {
    AcquireTotal    atomic.Int64
    AcquireErrors   atomic.Int64
    RateLimited     atomic.Int64
    BulkheadRejects atomic.Int64
    PoolWaits       atomic.Int64
    ReleaseDuration atomic.Int64  // суммарная наносекунды для средней
    DedupHits       atomic.Int64
    DedupMisses     atomic.Int64
    HealthChecks    atomic.Int64
    HealthErrors    atomic.Int64
}

type ManagerStats struct {
    Pool      connpool.PoolStats
    Bulkhead  bulkhead.BulkheadStats
    RateLimit RateLimitStats
    Metrics   MetricsSnapshot
}
```

**Stress test** (`stress_test.go`):

```go
func TestStress(t *testing.T) {
    // 200 горутин, 30 секунд, 3 partitions, rate limit, health check
    // Assertions: no race, no deadlock, no leak, metrics consistent
}
```

**Бенчмарк** (`bench_test.go`):

```go
func BenchmarkAcquireRelease(b *testing.B)
func BenchmarkAcquireReleaseConcurrent(b *testing.B)
func BenchmarkDedupDo(b *testing.B)
func BenchmarkFullPipeline(b *testing.B)
```

**Файл `main.go`** — демонстрация:
- Создание PoolManager со всеми компонентами
- Нагрузка: 100 горутин × 3 partitions × 10 секунд
- Вывод Stats каждую секунду
- Health check каждые 5 секунд
- Singleflight для идентичных запросов
- Graceful shutdown
- pprof endpoint для профилирования

## Требования

1. Pipeline Acquire: `rate limiter → bulkhead → pool → buffer`. Каждый шаг может отклонить: rate limit exceeded, partition full, pool exhausted, buffer allocation failure. Каждый отказ — конкретная ошибка и метрика.

2. Pipeline Release: `buffer → pool → bulkhead`. Обратный порядок. Каждый шаг — cleanup. Ошибка на одном шаге не должна блокировать остальные.

3. `ManagedConn.Release()` — idempotent через `atomic.Bool`. Двойной Release — no-op с warn log. `defer mc.Release()` — обязательный паттерн.

4. `DedupDo` — acquire conn, execute fn, release. Singleflight по key. Conn не разделяется между горутинами — каждая получает свой. Дедупликация — на уровне результата fn, не conn.

5. Stress test assertions:
    - `go test -race` — zero data races
    - Финальный `Stats().Pool.Active == 0` — все conn returned
    - `Stats().Metrics.AcquireTotal == AcquireErrors + successful releases` — balance
    - No goroutine leaks: `runtime.NumGoroutine()` before и after ≈ equal
    - Timeout errors < 10% total (system should cope)

6. Бенчмарки: `b.ReportAllocs()`, `b.RunParallel`. Ожидаемый overhead PoolManager vs raw Pool: <20% (rate limiter + bulkhead + metrics).

7. pprof: `import _ "net/http/pprof"` + HTTP server на отдельном порту. CPU profile, heap profile, goroutine dump — для диагностики bottlenecks.

8. Structured logging через `slog`: каждый Acquire/Release, ошибки, health check результаты. Log level configurable.

## Структура файлов

```
sync-primitives/
├── go.mod
├── main.go
├── semaphore/
│   ├── semaphore.go
│   └── weighted.go
├── safemap/
│   └── safemap.go
├── ratelimit/
│   └── ratelimit.go
├── connpool/
│   ├── pool.go
│   ├── conn.go
│   ├── options.go
│   └── stats.go
├── bufpool/
│   ├── buffer.go
│   ├── slice.go
│   └── bench_test.go
├── bulkhead/
│   ├── bulkhead.go
│   ├── partition.go
│   └── options.go
├── dedup/
│   └── singleflight.go
└── manager/
    ├── manager.go       # PoolManager
    ├── conn.go          # ManagedConn
    ├── metrics.go       # PoolMetrics, ManagerStats
    ├── options.go       # ManagerOptions
    ├── stress_test.go   # stress test
    └── bench_test.go    # benchmarks
```

## Ожидаемый вывод main.go

```
=== PoolManager Configuration ===
MaxConns: 20, MaxIdle: 10
MaxLifetime: 30m, MaxIdleTime: 5m
RateLimit: 100/sec, burst: 20
Partitions: critical(5), normal(10), background(3)
HealthCheck: every 5s
BufferPool: 4096 bytes

=== Starting load test (100 goroutines × 3 partitions × 10s) ===
[1s]  acquire=312  errors=12  rate_limited=8  bulkhead_reject=4  dedup_hits=23
[2s]  acquire=298  errors=5   rate_limited=3  bulkhead_reject=2  dedup_hits=31
[3s]  acquire=305  errors=8   rate_limited=5  bulkhead_reject=3  dedup_hits=27
...
[10s] acquire=301  errors=6   rate_limited=4  bulkhead_reject=2  dedup_hits=29

=== Health Check ===
[5s]  health check: 18/20 alive, 2 closed (idle timeout)
[10s] health check: 19/20 alive, 1 closed (max lifetime)

=== Final Stats ===
Total acquire: 3,047
Total errors: 83 (2.7%)
  Rate limited: 45
  Bulkhead rejected: 28
  Pool timeout: 10
Dedup hits: 271 (8.9% — 271 queries saved)
Pool: active=0, idle=10, created=47, closed=27
Health checks: 2, errors found: 3

=== Graceful Shutdown ===
Closing pool manager...
Rate limiter closed
Bulkhead closed
Pool closed (10 idle connections closed)
All resources released

=== Race detector: CLEAN ===
=== Goroutine leak check: PASS (before=4, after=4) ===

=== pprof available at http://localhost:6060/debug/pprof/ ===
```

## Эксперимент на разрушение

**Эксперимент 1: Goroutine leak detection.** Запустить stress test. Записать `runtime.NumGoroutine()` до и после. Если after > before + threshold — leak. Намеренно создать leak: убрать `defer mc.Release()` в одной горутине. Проверить: after > before. Найти leak через pprof goroutine dump: `go tool pprof http://localhost:6060/debug/pprof/goroutine`.

**Эксперимент 2: Deadlock под нагрузкой.** Изменить порядок acquire в одной горутине: pool → bulkhead (вместо bulkhead → pool). Запустить 200 горутин. Дождаться deadlock. Зафиксировать через pprof: `debug/pprof/goroutine?debug=2` — показывает стектрейсы всех заблокированных горутин. Circular wait виден в стектрейсах.

**Эксперимент 3: Performance regression.** Бенчмарк: PoolManager vs raw Pool.Acquire/Release. Ожидание: <20% overhead. Если >50% — профилировать через CPU pprof: `go test -bench=. -cpuprofile=cpu.prof`. `go tool pprof cpu.prof` → `top`, `web`, `list`. Найти bottleneck: rate limiter lock? bulkhead semaphore? metrics atomic? Оптимизировать.

## Вопросы до написания кода

1. Pipeline: rate limit → bulkhead → pool. Rate limit denied — ошибка немедленно. Bulkhead denied после rate limit passed — rate limit token потрачен впустую? Как учитывать?

2. `DedupDo` — acquire conn, fn, release. Conn внутри fn. Singleflight по key. Первый caller: acquire → fn(conn) → release → result. Followers: ждут result. Followers НЕ acquire conn. Одна conn для одного fn. Это корректно?

3. Stress test: 200 горутин × 30 секунд. Как проверить "нет deadlock" программно? Timeout на тест? Watchdog горутина?

4. `PoolMetrics` — все поля `atomic.Int64`. Почему не `sync.Mutex` + обычные int? Когда atomic лучше, когда mutex?

5. pprof: `import _ "net/http/pprof"`. Blank import. Что регистрирует пакет при импорте? Безопасно ли в production?

6. Graceful shutdown: Close rate limiter → Close bulkhead → Close pool. Порядок важен? Что если Close pool первым?