# Модуль 006 — Connection Pool Core

## Контекст

Пять предыдущих модулей построили примитивы: семафор (ограничение конкурентности), context-aware acquire (таймауты и отмена), weighted semaphore (неравные операции), SafeMap (потокобезопасный кэш), rate limiter (ограничение частоты). Этот модуль объединяет их в центральный компонент — connection pool.

Connection pool решает три задачи одновременно: (1) переиспользование ресурсов — создание соединения дорого (TCP handshake, TLS negotiation, authentication — десятки миллисекунд), переиспользование — дёшево, (2) ограничение количества — база данных не выдержит 10,000 одновременных соединений, пул ограничивает до N, (3) управление жизненным циклом — соединения стареют, ломаются, простаивают, их нужно проверять и заменять.

`database/sql` в Go уже содержит connection pool. Но он специфичен для SQL. Этот модуль строит generic pool — для любого ресурса: TCP соединений, HTTP клиентов, gRPC каналов, файловых дескрипторов.

## Что нужно реализовать

Новый пакет `connpool`.

**Интерфейс ресурса:**

```go
type Resource interface {
    Close() error
    IsAlive() bool
}
```

Любой тип реализующий `Resource` может управляться пулом. `Close()` — освободить ресурс. `IsAlive()` — проверить работоспособность (ping для DB, write test для TCP).

**Тип `Conn`** — обёртка вокруг ресурса с метаданными:

```go
type Conn struct {
    resource  Resource
    pool      *Pool
    createdAt time.Time
    lastUsed  time.Time
    useCount  int64
}
```

**Методы `Conn`:**

- `Resource() Resource` — доступ к underlying ресурсу
- `Release()` — вернуть в пул (не Close, а возврат)
- `Close() error` — закрыть соединение и не возвращать в пул (плохое соединение)
- `CreatedAt() time.Time`
- `LastUsed() time.Time`
- `UseCount() int64`
- `Age() time.Duration` — время с момента создания
- `IdleTime() time.Duration` — время с последнего использования

**Тип `Pool`:**

```go
type Pool struct {
    factory    Factory
    mu         sync.Mutex
    idle       []*Conn          // свободные соединения
    active     int              // количество выданных соединений
    maxSize    int              // максимальное количество соединений
    maxIdle    int              // максимальное количество idle
    maxLifetime time.Duration   // максимальный возраст соединения
    maxIdleTime time.Duration   // максимальное время простоя
    waitQueue  []*waiter        // очередь ожидающих
    closed     bool
    done       chan struct{}
    sem        *semaphore.Semaphore  // из модуля 001-002
}

type Factory func(ctx context.Context) (Resource, error)

type waiter struct {
    conn chan *Conn
    ctx  context.Context
}
```

**Конструктор `New(factory Factory, opts ...Option) *Pool`.**

**Options:**

```go
WithMaxSize(n int) Option           // default: 10
WithMaxIdle(n int) Option           // default: 5
WithMaxLifetime(d time.Duration)    // default: 30min
WithMaxIdleTime(d time.Duration)    // default: 5min
WithHealthCheck(interval time.Duration) // default: 0 (disabled)
```

**Методы Pool:**

- `Acquire(ctx context.Context) (*Conn, error)` — получить соединение. Алгоритм: (1) проверить idle → вернуть если есть живое, (2) если active < maxSize → создать новое, (3) если active == maxSize → встать в очередь ожидания.

- `Release(conn *Conn)` — вернуть в пул. Если соединение живое и не просрочено → в idle. Иначе → Close.

- `Close() error` — закрыть пул: закрыть все idle соединения, отклонить всех ожидающих, запретить новые Acquire.

- `Stats() PoolStats` — статистика: active, idle, total, wait count, wait duration.

- `Len() int` — total connections (active + idle).

**Тип `PoolStats`:**

```go
type PoolStats struct {
    Active      int
    Idle        int
    Total       int
    MaxSize     int
    WaitCount   int64         // суммарно ожидавших
    WaitDuration time.Duration // суммарное время ожидания
    CreatedTotal int64         // всего создано
    ClosedTotal  int64         // всего закрыто
}
```

**Фоновая горутина health check** (если настроена):
- Периодически проверяет idle соединения: `IsAlive()`, maxLifetime, maxIdleTime
- Закрывает просроченные
- Не блокирует Acquire/Release

**Файл `main.go`** — демонстрация с mock Resource:

```go
type MockConn struct {
    id     int
    alive  bool
    closed bool
}
func (c *MockConn) Close() error { c.closed = true; return nil }
func (c *MockConn) IsAlive() bool { return c.alive && !c.closed }
```

- Создание пула с factory
- Acquire → use → Release цикл
- Конкурентный доступ: 50 горутин
- Исчерпание пула: ожидание в очереди
- Timeout на Acquire
- Stats вывод
- Закрытие пула

## Требования

1. `Acquire` — FIFO очередь ожидающих (из модуля 003). Первый запросивший — первый получит. Не "кто быстрее захватит мьютекс".

2. `Release` проверяет соединение перед возвратом в idle: `IsAlive()`, возраст < maxLifetime, idle time сбрасывается. Если проверка не пройдена — соединение закрывается, счётчик active уменьшается, проверяется очередь ожидающих (может нужно создать новое).

3. `idle` — слайс как LIFO стек: последнее возвращённое — первое выданное. LIFO для idle потому что недавно использованное соединение с большей вероятностью рабочее. FIFO для waiters (fairness), LIFO для idle connections (freshness).

4. Создание нового соединения (`factory(ctx)`) — вне мьютекса. Factory может быть медленной (TCP connect, TLS). Мьютекс нельзя держать во время IO. Паттерн: под мьютексом проверить "можно создать" (active < maxSize) → active++, unlock → create вне lock → если ошибка: lock, active--.

5. `Conn.Release()` — может вызываться только один раз. Повторный Release — panic или no-op с логом. Отслеживается через флаг `released bool` в Conn.

6. Health check горутина запускается в конструкторе если `WithHealthCheck` настроен. Завершается при `Close()`. Тикер + select с `done` каналом.

7. `Close()` через `sync.Once`. Закрывает все idle. Waiting горутины получают ошибку. Active соединения — не закрываются принудительно (они у клиентского кода), но при Release после Close — закрываются.

8. Race detector на всех демонстрациях.

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
└── connpool/
    ├── pool.go        # Pool, Acquire, Release, Close
    ├── conn.go        # Conn, Resource interface
    ├── options.go     # functional options
    └── stats.go       # PoolStats
```

## Ожидаемый вывод main.go

```
=== Basic Acquire/Release ===
Acquired conn #1 (created new)
Used conn #1, releasing...
Acquired conn #1 (reused from idle)

=== Concurrent access (50 goroutines, maxSize=5) ===
[00:00.000] 5 connections created, 45 goroutines waiting
[00:00.102] conn released → waiter served
[00:00.105] conn released → waiter served
...
All 50 goroutines completed
Max concurrent: 5 ✓

=== Timeout ===
Pool full (5/5 active, 0 idle)
Acquire with 100ms timeout: context deadline exceeded

=== Stats ===
Active: 0, Idle: 5, Total: 5, MaxSize: 10
WaitCount: 45, WaitDuration: 2.3s
CreatedTotal: 5, ClosedTotal: 0

=== MaxLifetime ===
Conn #1 age: 31m (maxLifetime: 30m)
Release → conn closed (too old), new conn created
Conn #6 acquired (fresh)

=== MaxIdleTime ===
Conn #2 idle: 6m (maxIdleTime: 5m)
Health check → conn closed (idle too long)
Idle count: 4 → 3

=== Pool Close ===
Closing pool...
3 idle connections closed
2 waiting goroutines: pool closed error
Acquire after close: pool closed
```

## Эксперимент на разрушение

**Эксперимент 1: Factory вне мьютекса.** Реализовать Acquire с factory ПОД мьютексом:

```go
func (p *Pool) BrokenAcquire(ctx context.Context) (*Conn, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    // ... check idle ...
    resource, err := p.factory(ctx)  // IO под мьютексом!
    // ...
}
```

Factory занимает 100ms (TCP connect). 10 горутин Acquire одновременно. Зафиксировать: все 10 последовательно (мьютекс заблокирован 10×100ms = 1s). С factory вне мьютекса: все 10 параллельно (100ms total). 10x разница.

**Эксперимент 2: LIFO vs FIFO для idle.** FIFO idle: `idle[0]` — самое старое (давно не использованное). LIFO: `idle[len-1]` — самое свежее. С maxIdleTime=5m: FIFO → первое выданное — самое старое, может быть stale. LIFO → первое выданное — самое свежее, вероятно живое. Старые соединения в хвосте idle → вытесняются health check. Реализовать оба, измерить: (a) % stale connections при Acquire, (b) количество Close+Recreate циклов.

**Эксперимент 3: Release после Close пула.** Пул закрыт. Горутина всё ещё работает с Conn, полученным до Close. Горутина вызывает `conn.Release()`. Что происходит? Conn возвращается в закрытый пул? Или закрывается?

Зафиксировать: Release проверяет `p.closed`. Если true → Close соединение (не возвращать в idle). Active count уменьшается. Если waiters есть (быть не должно после Close, но race) — не создавать новое.

## Вопросы до написания кода

1. `database/sql.DB` — встроенный connection pool Go. Какие из наших Options есть в `sql.DB`? (`SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxLifetime`, `SetConnMaxIdleTime`). Совпадение дизайна — не случайность.

2. LIFO для idle, FIFO для waiters. Почему разные стратегии для разных очередей? Что если оба FIFO? Что если оба LIFO?

3. Factory вне мьютекса. Между "решили создать" (active++) и "создали" — окно. Factory вернула ошибку. Нужно active--. Но за это время другая горутина решила не создавать (видела active==maxSize). Потерян один слот?

4. `Conn.Release()` vs `Conn.Close()`. Когда клиент должен вызвать Close вместо Release? Привести конкретные сценарии.

5. Health check горутина + Acquire горутина — обе обращаются к idle слайсу. Как синхронизировать? Один мьютекс? Отдельный?

6. Pool с maxSize=1. Acquire → Release → Acquire. Второй Acquire получает то же соединение (LIFO idle). А если maxSize=1 и две горутины: A Acquire → A работает → B Acquire (блокируется) → A Release → B получает conn A. Работает ли это?