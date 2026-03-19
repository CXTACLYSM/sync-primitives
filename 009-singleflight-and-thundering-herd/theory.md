# Модуль 009 — Theory: Singleflight, errgroup и дедупликация запросов

## Thundering Herd: проблема

### Сценарий

Cache key "user:42" expires. Одновременно 100 горутин обрабатывают запросы для user 42:

```
goroutine 1: cache.Get("user:42") → miss → db.Query("SELECT * FROM users WHERE id=42")
goroutine 2: cache.Get("user:42") → miss → db.Query("SELECT * FROM users WHERE id=42")
...
goroutine 100: cache.Get("user:42") → miss → db.Query("SELECT * FROM users WHERE id=42")
```

100 идентичных запросов к DB. 100 connection pool slots. DB делает одну работу 100 раз. Результат один. Waste: 99 запросов × query time × connection pool slots.

При 10,000 req/sec и cache TTL 1 second: каждую секунду — burst из ~10,000 идентичных DB queries при cache miss. DB не выдержит.

### Почему это "thundering herd"

Метафора: стадо бизонов. Один шум — все несутся в одном направлении одновременно. Cache miss — "шум". Все горутины — "стадо". Database — куда они несутся.

Варианты название: thundering herd, cache stampede, dog-pile effect. Все описывают одну проблему.

## Singleflight: решение

### Принцип

Первый запрос с ключом K выполняет функцию. Все последующие запросы с тем же ключом K, пришедшие пока первый выполняется, ждут его результат. Результат разделяется.

```
goroutine 1: Do("user:42", query) → executes query (leader)
goroutine 2: Do("user:42", query) → waits for goroutine 1
...
goroutine 100: Do("user:42", query) → waits for goroutine 1

goroutine 1 completes → result shared with all 100
```

1 query вместо 100. 1 connection вместо 100. 1x DB load вместо 100x.

### API

```go
type Group struct {
    mu    sync.Mutex
    calls map[string]*call
}

func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool)
```

`key` — идентификатор запроса. Одинаковый key = одинаковый запрос = дедупликация. `fn` — функция для выполнения. `shared` — true если результат разделён (вызывающий не был leader).

## Реализация Do: пошагово

### Алгоритм

```go
func (g *Group) Do(key string, fn func() (any, error)) (any, error, bool) {
    g.mu.Lock()
    
    // Lazy init
    if g.calls == nil {
        g.calls = make(map[string]*call)
    }
    
    // Есть in-flight call — ждать
    if c, ok := g.calls[key]; ok {
        g.mu.Unlock()
        c.wg.Wait()         // блокировка до завершения leader
        return c.val, c.err, true  // shared=true
    }
    
    // Первый — стать leader
    c := &call{}
    c.wg.Add(1)
    g.calls[key] = c
    g.mu.Unlock()
    
    // Выполнить fn (вне мьютекса!)
    c.val, c.err = fn()
    c.wg.Done()  // разблокировать всех ожидающих
    
    // Удалить из map
    g.mu.Lock()
    delete(g.calls, key)
    g.mu.Unlock()
    
    return c.val, c.err, false  // shared=false (leader)
}
```

### Почему WaitGroup а не channel

`WaitGroup.Wait()` — множество горутин ждут одно событие. `wg.Done()` — broadcast (все Wait разблокированы). Channel: `close(ch)` — тоже broadcast. Оба работают.

WaitGroup проще: не нужно создавать канал, не нужно определять тип. Два вызова: `Add(1)`, `Done()`. Channel: `make(chan struct{})`, `close(ch)`, `<-ch`. Три операции.

Channel лучше для `DoChan` — нужно вернуть канал вызывающему для select. WaitGroup не интегрируется с select. `x/sync/singleflight` внутри использует WaitGroup для Do, канал для DoChan.

### Удаление из map после fn

`delete(g.calls, key)` после `c.wg.Done()`. Следующий `Do(key)` — новый call, новый fn. Singleflight не кэширует результат — только дедуплицирует in-flight.

Порядок: `wg.Done()` ДО `delete`. Между Done и delete: новый Do видит call в map → Wait → получает результат (уже готовый, Wait возвращается немедленно). Это корректно но redundant: горутина подождала 0ns и получила stale call. После delete: новый Do → новый fn. Альтернатива: `delete` ДО `wg.Done()`:

```go
g.mu.Lock()
delete(g.calls, key)
g.mu.Unlock()
c.val, c.err = fn()  // ??? нет, fn уже выполнен
c.wg.Done()
```

Нет — delete должен быть ПОСЛЕ fn, но порядок delete и Done внутри lock можно варьировать. `x/sync/singleflight`: delete under lock, потом wg.Done:

```go
g.mu.Lock()
delete(g.calls, key)
g.mu.Unlock()
c.wg.Done()
```

Между delete и Done: новый Do с тем же key → не найдёт в map → станет новым leader → запустит fn параллельно с Done old. Ожидающие old call — всё ещё ждут Done → получат результат. Это корректно: old waiters получают old result, new Do получает fresh result.

## DoChan: неблокирующий вариант

### Зачем

`Do` блокирует вызывающую горутину. Нет способа отменить ожидание (нет context). `DoChan` возвращает канал — интегрируется с `select`:

```go
ch := g.DoChan("user:42", fetchUser)

select {
case result := <-ch:
    // result.Val, result.Err, result.Shared
case <-ctx.Done():
    // timeout / cancel
}
```

### Реализация

```go
func (g *Group) DoChan(key string, fn func() (any, error)) <-chan Result {
    ch := make(chan Result, 1)  // buffered 1: sender не блокируется
    
    g.mu.Lock()
    if g.calls == nil {
        g.calls = make(map[string]*call)
    }
    
    if c, ok := g.calls[key]; ok {
        g.mu.Unlock()
        // Ожидающий: горутина ждёт и отправляет в ch
        go func() {
            c.wg.Wait()
            ch <- Result{Val: c.val, Err: c.err, Shared: true}
        }()
        return ch
    }
    
    c := &call{}
    c.wg.Add(1)
    g.calls[key] = c
    g.mu.Unlock()
    
    // Leader: горутина выполняет fn и отправляет в ch
    go func() {
        c.val, c.err = fn()
        c.wg.Done()
        
        g.mu.Lock()
        delete(g.calls, key)
        g.mu.Unlock()
        
        ch <- Result{Val: c.val, Err: c.err, Shared: false}
    }()
    
    return ch
}
```

Каждый вызов DoChan создаёт горутину. Горутина ждёт (WaitGroup или выполняет fn) и отправляет результат в канал. Caller получает канал немедленно, результат — асинхронно.

## Forget: сброс in-flight

### Зачем

Fn завис (медленный запрос, network timeout). 100 горутин ждут. Без Forget: все 100 ждут пока fn завершится (или timeout на уровне caller). С Forget:

```go
go func() {
    time.Sleep(5 * time.Second)
    g.Forget("slow-key")  // сбросить
}()
```

После Forget: следующий Do("slow-key") — новый leader, новый fn (параллельно со старым). Старый fn продолжает, его waiters получат его результат. Новые callers — получат результат нового fn. Два fn для одного key одновременно — это explicit opt-in через Forget.

### Реализация

```go
func (g *Group) Forget(key string) {
    g.mu.Lock()
    delete(g.calls, key)
    g.mu.Unlock()
}
```

Просто delete из map. Текущий call (`*call`) не уничтожается — waiters всё ещё ссылаются на него через `c.wg.Wait()`. Fn продолжает работу. `wg.Done()` разблокирует waiters. Но delete уже удалил call из map → `delete(g.calls, key)` в Do leader — no-op (уже удалён). Корректно.

## errgroup: параллельные операции

### Базовый API

```go
import "golang.org/x/sync/errgroup"

g, ctx := errgroup.WithContext(parentCtx)

g.Go(func() error {
    return checkConn1(ctx)
})
g.Go(func() error {
    return checkConn2(ctx)
})

err := g.Wait()  // ждёт все горутины, возвращает первую ошибку
```

### WithContext: cancel на первую ошибку

`errgroup.WithContext(parent)` создаёт derived context. Первая ошибка из `g.Go` — `cancel()` на derived context. Остальные горутины видят `ctx.Done()` и могут завершиться рано.

Важно: `Wait()` ждёт ВСЕ горутины, не только до первой ошибки. Даже после cancel — горутины должны завершиться (проверяя ctx.Done). Если горутина игнорирует ctx — Wait висит.

### SetLimit: ограничение конкурентности

```go
g.SetLimit(5)  // не более 5 горутин одновременно

for _, conn := range conns {
    conn := conn
    g.Go(func() error {  // блокируется если 5 уже работают
        return conn.Resource().IsAlive()
    })
}
```

Внутри: семафор (channel-based, аналог модуля 001). `Go` делает `sem.Acquire` перед запуском горутины. Горутина завершается → `sem.Release`. `SetLimit(5)` + 100 `Go` = 100 горутин запланировано, 5 работают одновременно, 95 ждут.

### errgroup для health check

```go
func (p *Pool) HealthCheckParallel(ctx context.Context) error {
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(5)  // не более 5 параллельных проверок
    
    p.mu.RLock()
    conns := slices.Clone(p.idle)
    p.mu.RUnlock()
    
    for _, conn := range conns {
        conn := conn
        g.Go(func() error {
            if !conn.Resource().IsAlive() {
                return fmt.Errorf("conn %d dead", conn.id)
            }
            return nil
        })
    }
    
    return g.Wait()
}
```

Параллельный health check с ограничением конкурентности. Первая мёртвая conn → cancel context → остальные проверки прерываются (если проверяют ctx). Wait возвращает первую ошибку.

## Singleflight: где применять в connection pool

### Подходящие случаи

1. **Health check дедупликация.** 10 горутин проверяют "жив ли backend" одновременно. Один ping, все получают ответ.

2. **DNS resolve.** Factory вызывает DNS. 10 параллельных factory → 10 DNS queries к одному host. Singleflight: 1 DNS query.

3. **Кэш поверх pool.** `GetUser(id)` → conn.Query. Singleflight по user ID: один запрос, все получают User.

### Неподходящие случаи

1. **Connection acquire.** Conn — mutable resource, нельзя разделить. Singleflight вернёт один conn нескольким горутинам → data race.

2. **Write operations.** INSERT user — каждый INSERT с разными данными. Singleflight по user ID = один INSERT, остальные думают что их данные записаны.

3. **Operations with side effects.** Любая операция которая не идемпотентна — не подходит для singleflight.

### Правило

Singleflight подходит для **идемпотентных read-only** операций где результат одинаков для всех callers с одним ключом. `f(key) = const` для фиксированного key в момент вызова.

## Вопросы для самопроверки

1. `Do` удаляет call из map после fn. Между delete и Done: новый Do → новый leader. Старый waiters → всё ещё Wait на старом call. Два fn для одного key. Это race?

2. `fn` возвращает `*User` (pointer). 100 горутин получают тот же pointer. Горутина #50 модифицирует `user.Name = "hacked"`. Остальные видят "hacked". Как защититься?

3. `errgroup.Wait()` возвращает первую ошибку. Но горутины могут вернуть несколько ошибок. Как получить все ошибки?

4. `errgroup.SetLimit(1)` — sequential execution. Зачем errgroup для sequential? Можно просто цикл.

5. Singleflight для cache: `Do(key, fetchFromDB)`. fn занимает 100ms. 1000 горутин ждут. Fn completed → 1000 разблокированы → cache set. Следующий burst через 1 секунду (cache hit). Но: 1000 горутин разблокированы одновременно → каждая делает cache.Set с одним результатом. 1000 cache.Set для одного key. Проблема?