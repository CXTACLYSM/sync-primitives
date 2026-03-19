# Модуль 009 — Singleflight & Thundering Herd

## Контекст

Кэш инвалидировался. 100 горутин одновременно запрашивают одни и те же данные. Каждая вызывает `pool.Acquire → DB query → pool.Release`. 100 идентичных запросов к базе. 100 connection pool slots заняты. База делает одну и ту же работу 100 раз. Ответ один и тот же.

Это thundering herd — стадо несётся на один ресурс одновременно. Singleflight решает: первый запрос выполняется, остальные 99 ждут его результат. Один запрос к базе, один connection pool slot, 100 горутин получают ответ.

`golang.org/x/sync/singleflight` — стандартная реализация. `golang.org/x/sync/errgroup` — параллельные операции с отменой при первой ошибке. Оба пакета интегрируются в connection pool как слои защиты.

## Что нужно реализовать

Два компонента: собственная реализация singleflight для понимания механизма, и интеграция `x/sync` пакетов в connection pool.

**Собственный `singleflight` (пакет `dedup`):**

```go
type Group struct {
    mu    sync.Mutex
    calls map[string]*call
}

type call struct {
    wg  sync.WaitGroup
    val any
    err error
}
```

**Методы:**

- `Do(key string, fn func() (any, error)) (any, error, bool)` — выполнить fn для ключа. Если вызов с таким ключом уже in-flight — ждать его результат. Третье возвращаемое значение: `shared` — true если результат разделён между несколькими вызывающими.

- `DoChan(key string, fn func() (any, error)) <-chan Result` — неблокирующий вариант: возвращает канал, результат придёт когда fn завершится.

- `Forget(key string)` — удалить запись о in-flight вызове. Следующий Do с тем же ключом запустит fn заново (не дождётся текущего).

**Тип `Result`:**

```go
type Result struct {
    Val    any
    Err    error
    Shared bool
}
```

**Интеграция с connpool — `CachedPool`:**

```go
type CachedPool struct {
    pool  *connpool.Pool
    group dedup.Group
}

func (cp *CachedPool) AcquireDedup(ctx context.Context, key string) (*connpool.Conn, error)
```

При одновременных Acquire с одинаковым key — только один вызывает `pool.Acquire`, остальные получают тот же conn... Стоп. Conn нельзя разделить — он привязан к одному пользователю. Singleflight для connection pool применяется иначе: дедупликация **factory** (создание нового соединения), не acquire.

**Правильная интеграция:** когда pool нужно создать новое соединение (idle пуст, active < maxSize), singleflight дедуплицирует factory вызовы к одному endpoint:

```go
func (p *Pool) createConn(ctx context.Context) (Resource, error) {
    v, err, _ := p.sf.Do(p.endpoint, func() (any, error) {
        return p.factory(ctx)
    })
    if err != nil {
        return nil, err
    }
    return v.(Resource), nil
}
```

Но: это разделит один conn между несколькими горутинами — неправильно. Singleflight подходит для **read-only** операций с кэшируемым результатом, не для mutable ресурсов.

**Правильное применение singleflight в context pool:**
- Дедупликация health check: 10 горутин проверяют is backend alive — один ping, все получают результат.
- Дедупликация DNS resolve: factory вызывает DNS — один resolve per host.
- Кэш поверх pool: `GetUser(id)` через conn — один запрос, результат кэшируется.

**errgroup интеграция — `ParallelHealthCheck`:**

```go
func (p *Pool) HealthCheckAll(ctx context.Context) error {
    g, ctx := errgroup.WithContext(ctx)
    
    p.mu.RLock()
    conns := slices.Clone(p.idle)
    p.mu.RUnlock()
    
    for _, conn := range conns {
        conn := conn
        g.Go(func() error {
            if !conn.Resource().IsAlive() {
                return fmt.Errorf("conn %d: dead", conn.id)
            }
            return nil
        })
    }
    
    return g.Wait()
}
```

**Файл `main.go`** — демонстрация:
- Singleflight: 50 горутин запрашивают один key — fn вызывается 1 раз
- DoChan: неблокирующий вариант
- Forget: сброс in-flight
- errgroup: параллельный health check 10 connections
- errgroup с cancel: первая ошибка отменяет остальные
- Бенчмарк: 1000 горутин с singleflight vs без

## Требования

1. `Do` с `sync.WaitGroup`: первый вызов создаёт `call{wg: initialized}`, `wg.Add(1)`, запускает fn. Последующие вызовы с тем же key находят существующий call, `wg.Wait()`. Fn завершается → `wg.Done()` → все ожидающие разблокированы.

2. `call` хранит результат `(val, err)`. Все ожидающие получают один и тот же `val` и `err`. Если fn паникует — паника propagates к ПЕРВОМУ вызывающему. Остальные получают recovered error.

3. `Do` удаляет call из map после завершения fn. Следующий Do с тем же key — новый вызов. Это "cache for in-flight only" — не persistent cache.

4. `Forget` удаляет call ДО завершения fn. Следующий Do запустит fn параллельно с текущим. Два одновременных fn для одного key. Используется для retry: текущий fn завис → Forget → новый Do → свежий fn.

5. `DoChan` возвращает `<-chan Result`. Не блокирует вызывающего. Результат приходит когда fn завершится. Для select с ctx.Done().

6. `errgroup.WithContext`: возвращает group и derived context. Первая ошибка из `g.Go` отменяет context. Остальные горутины видят `ctx.Done()` и могут завершиться рано. `g.Wait()` ждёт ВСЕ горутины и возвращает первую ошибку.

7. `errgroup.SetLimit(n)` — ограничить конкурентность горутин. `g.SetLimit(5)`: не более 5 горутин одновременно. `g.Go` блокируется если лимит достигнут. Это семафор внутри errgroup.

8. Race detector обязателен.

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
└── dedup/
    └── singleflight.go
```

## Ожидаемый вывод main.go

```
=== Singleflight: 50 goroutines, same key ===
fn called: 1 time (not 50!)
All 50 goroutines received same result: "data-for-user-42"
Shared: true for 49 goroutines

=== Singleflight: different keys ===
fn called: 3 times (keys: "a", "b", "c")
Each key → separate fn execution

=== DoChan ===
Submitted request, not blocked
Doing other work...
Result arrived: "async-result"

=== Forget ===
Do("slow-key") started (takes 5s)
After 1s: Forget("slow-key")
Do("slow-key") again → NEW fn started (parallel with first)
First completed: "result-1"
Second completed: "result-2" (different execution)

=== errgroup: parallel health check ===
Checking 10 connections in parallel...
All healthy: 8/10
Errors: conn-3 dead, conn-7 dead
First error: conn-3 dead (returned by Wait)
Duration: 15ms (parallel, not 150ms sequential)

=== errgroup with cancel ===
5 goroutines, first fails at 50ms
Goroutine 2: cancelled (ctx.Done) at 50ms
Goroutine 3: cancelled (ctx.Done) at 51ms
Total duration: ~50ms (not 250ms)

=== errgroup with limit ===
10 tasks, limit=3
Max concurrent: 3 ✓

=== Benchmark: singleflight vs no dedup ===
1000 goroutines, same query:
  Without: 1000 DB queries, 2.3s total, pool exhausted
  With:    1 DB query, 0.05s total, pool barely used
  Speedup: 46x
```

## Эксперимент на разрушение

**Эксперимент 1: Panic в fn.** Singleflight: 10 горутин, одна fn которая паникует. Первый вызывающий — кто получит panic? Остальные 9 — что получат? Зафиксировать: panic propagates к caller who initiated fn. Остальные получают recovered result (или тоже panic, зависит от реализации). `x/sync/singleflight`: panic propagates к ВСЕМ ожидающим. Своя реализация: выбрать стратегию и обосновать.

**Эксперимент 2: Thundering herd на cache miss.** Кэш с TTL 1 секунда. 1000 req/sec. При TTL expire: 1000 горутин одновременно обнаруживают cache miss → 1000 запросов к DB. Без singleflight: 1000 DB queries. С singleflight: 1 DB query, 999 ждут. Измерить: pool active connections пик, DB query count, p99 latency.

**Эксперимент 3: Stale singleflight.** fn занимает 10 секунд (медленный запрос). 100 горутин ждут результат. Fn возвращает stale data (DB отвечала 10 секунд, данные уже изменились). Все 100 получают stale data. Без singleflight: каждая горутина бы получила свежие данные (sequential queries видят updates). Trade-off: freshness vs efficiency. Как решить? `Forget` + retry? TTL на singleflight call?

## Вопросы до написания кода

1. `singleflight.Do` — fn выполняется ОДНОЙ горутиной. Результат разделяется. Если fn мутирует результат (возвращает pointer, потом модифицирует) — все получатели видят мутацию. Как защититься?

2. `sync.WaitGroup` в call: `wg.Add(1)` при создании, `wg.Done()` при завершении fn. Все ожидающие: `wg.Wait()`. Почему WaitGroup а не канал? Когда канал лучше?

3. `Forget(key)` удаляет call из map. Текущий fn продолжает работу. Новый Do запускает fn параллельно. Два fn для одного key одновременно. Это нарушение контракта "один fn для ключа"? Или Forget — explicit opt-out?

4. `errgroup.WithContext` — первая ошибка отменяет context. Но `Wait` ждёт ВСЕ горутины. Зачем ждать если уже есть ошибка? Что если горутина игнорирует ctx.Done?

5. `errgroup.SetLimit(5)` — семафор. Это тот же семафор что в модуле 001? Или другая реализация?

6. Singleflight для write операций (INSERT, UPDATE). 10 горутин INSERT одну запись. Singleflight: 1 INSERT. Остальные 9 думают что INSERT выполнен. Но: каждая горутина имела свои данные для INSERT. Singleflight подходит для writes?