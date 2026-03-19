# Модуль 006 — Theory: Connection Pool, lifecycle и конкурентный доступ

## Зачем пул соединений

### Стоимость создания

TCP соединение к PostgreSQL: DNS resolve (~1ms), TCP handshake (1 RTT, ~0.5ms LAN, ~50ms WAN), TLS handshake (1-2 RTT), PostgreSQL authentication (~1ms). Суммарно: 3-100ms. Один SQL запрос: 0.1-10ms. Создание соединения может стоить в 10-100 раз больше самого запроса.

Пул переиспользует: создание один раз, использование тысячи раз. Amortized cost создания → ~0.

### Ограничение ресурсов

PostgreSQL `max_connections = 100`. Без пула: 1000 горутин × 1 соединение = 1000 — PostgreSQL отклоняет. С пулом: 1000 горутин, maxSize=20 — 20 соединений, 980 горутин ожидают. PostgreSQL не перегружен.

### Lifecycle management

Соединения деградируют: сетевые проблемы (half-open TCP), серверные таймауты (PostgreSQL `idle_in_transaction_session_timeout`), firewall drops (NAT timeout). Без health check: "мёртвое" соединение из пула → ошибка у клиента → retry → ещё одно мёртвое → каскад ошибок. С health check: мёртвые вычищаются, клиент получает живые.

## Архитектура Pool

### Состояния соединения

```
                    ┌─────────┐
    factory() ────▶│ Active  │◀──── Acquire()
                    └────┬────┘
                         │ Release()
                    ┌────▼────┐
                    │  Idle   │──── health check → Close()
                    └────┬────┘
                         │ Acquire() (reuse)
                    ┌────▼────┐
                    │ Active  │
                    └────┬────┘
                         │ Close() or maxLifetime
                    ┌────▼────┐
                    │ Closed  │
                    └─────────┘
```

Три состояния: **Active** (выдано клиенту), **Idle** (в пуле, ждёт переиспользования), **Closed** (уничтожено). Переходы: factory→Active, Release→Idle, Acquire(idle)→Active, health/lifetime→Closed.

### Acquire: алгоритм детально

```
Acquire(ctx):
1. Lock
2. Если closed → unlock, return ErrPoolClosed
3. Пока idle не пуст:
   a. Взять последний (LIFO)
   b. Проверить maxLifetime, maxIdleTime
   c. Если просрочен → Close, active--, continue
   d. Если !IsAlive() → Close, active--, continue
   e. Обновить lastUsed, useCount, active++
   f. Unlock, return conn
4. Если active < maxSize:
   a. active++ (зарезервировать слот)
   b. Unlock
   c. resource, err = factory(ctx)  ← вне мьютекса!
   d. Если err: Lock, active--, пробудить waiter, unlock, return err
   e. Создать Conn, return
5. Создать waiter{conn: make(chan *Conn, 1), ctx: ctx}
6. Добавить в очередь waiters
7. Unlock
8. select { case conn := <-waiter.conn: return conn; case <-ctx.Done(): remove waiter, return err }
```

### Release: алгоритм

```
Release(conn):
1. Если conn.released → panic или log + return
2. conn.released = true
3. Lock
4. active--
5. Если closed → Close resource, check waiters (shouldn't be any), unlock, return
6. Если !conn.IsAlive() || age > maxLifetime:
   a. Close resource
   b. Проверить waiters: если есть и active < maxSize → создать новое (вне lock)
   c. Unlock, return
7. Если len(waiters) > 0:
   a. Взять первого waiter (FIFO)
   b. conn.lastUsed = now, active++
   c. waiter.conn <- conn (отправить в канал waiter)
   d. Unlock, return
8. Если len(idle) < maxIdle:
   a. conn.lastUsed = now
   b. Добавить в idle (append — LIFO: последний в слайсе)
   c. Unlock, return
9. Иначе (idle полон):
   a. Close resource
   b. Unlock, return
```

### Factory вне мьютекса

Критический паттерн: IO операция (factory) не должна выполняться под мьютексом.

```go
// Под мьютексом: принять решение
p.mu.Lock()
if p.active < p.maxSize {
    p.active++  // зарезервировать слот
    p.mu.Unlock()
} else {
    // ... wait queue
}

// Вне мьютекса: IO
resource, err := p.factory(ctx)
if err != nil {
    p.mu.Lock()
    p.active--  // откатить резервацию
    p.notifyWaitersLocked()
    p.mu.Unlock()
    return nil, err
}

// Создать Conn
conn := &Conn{resource: resource, pool: p, createdAt: time.Now(), lastUsed: time.Now()}
return conn, nil
```

Между `active++` и factory — окно. Другие горутины видят `active == maxSize` и ждут. Если factory провалится — `active--` и notification waiters. Один из waiters создаст новое соединение. Транзакция: reserve → attempt → rollback if failed.

## LIFO idle vs FIFO waiters

### LIFO для idle: freshness

Idle connections — стек (LIFO). Последнее возвращённое — первое выданное. Зачем:

1. **Свежесть.** Недавно использованное соединение с большей вероятностью живое. TCP keepalive не истёк, сервер не закрыл. Старое (давно idle) — может быть half-open.

2. **Natural eviction.** Старые соединения опускаются на дно стека. Если нагрузка стабильна — дно стека не используется. Health check закрывает их (maxIdleTime). Пул "сжимается" естественно.

3. **Cache locality.** На стороне сервера (DB): недавно использованное соединение может иметь "тёплые" prepared statements, кэши. Reuse — переиспользование этого тепла.

### FIFO для waiters: fairness

Waiting goroutines — очередь (FIFO). Первый запросивший — первый получит. Зачем:

1. **No starvation.** LIFO для waiters: новые горутины обслуживаются раньше старых → стоящие долго голодают.

2. **Predictable latency.** Время в очереди пропорционально позиции. Первый ждёт меньше всех.

3. **Аналог модуля 003.** Strict FIFO из weighted semaphore.

### Реализация LIFO idle через слайс

```go
// Push (Release): добавить в конец
p.idle = append(p.idle, conn)

// Pop (Acquire): взять с конца
n := len(p.idle)
conn := p.idle[n-1]
p.idle[n-1] = nil   // обнулить для GC
p.idle = p.idle[:n-1]
```

Слайс как стек: `append` = push, `s[len-1]` + truncate = pop. O(1) обе операции.

## Waiter queue: channel per waiter

### Почему channel per waiter

Каждый waiter получает свой `chan *Conn` (buffered 1). Release отправляет conn в канал конкретного waiter (первого в FIFO). Горутина waiter ждёт в select:

```go
select {
case conn := <-waiter.conn:
    return conn, nil
case <-ctx.Done():
    // cancel: удалить из очереди
    return nil, ctx.Err()
}
```

Почему не один общий канал? Общий канал не гарантирует FIFO: при нескольких ready receivers Go выбирает случайно. Индивидуальный канал: Release точно знает кому отправить (первый в очереди).

Почему buffered(1)? Release отправляет conn в waiter.conn под мьютексом. Если unbuffered — Release блокируется до тех пор пока waiter не прочитает. Но waiter может быть между итерациями select (context cancel проверка). С buffered(1): send не блокируется, waiter получит при следующем select.

### Cancel waiter

```go
case <-ctx.Done():
    p.mu.Lock()
    // Удалить waiter из очереди
    for i, w := range p.waiters {
        if w == myWaiter {
            p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
            break
        }
    }
    p.mu.Unlock()
    
    // Race: conn мог уже быть отправлен в waiter.conn
    select {
    case conn := <-myWaiter.conn:
        // Уже получили conn — вернуть в пул
        conn.Release()
    default:
        // Conn не отправлен — OK
    }
    
    return nil, ctx.Err()
```

Race между Release (send conn) и cancel: после удаления из очереди проверить канал — если conn уже отправлен, вернуть его (Release). Без этой проверки — conn потерян.

## Health Check

### Фоновая горутина

```go
func (p *Pool) startHealthCheck(interval time.Duration) {
    go func() {
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                p.checkIdle()
            case <-p.done:
                return
            }
        }
    }()
}

func (p *Pool) checkIdle() {
    p.mu.Lock()
    var toClose []*Conn
    alive := p.idle[:0]  // reuse backing array
    for _, conn := range p.idle {
        if conn.Age() > p.maxLifetime || conn.IdleTime() > p.maxIdleTime || !conn.resource.IsAlive() {
            toClose = append(toClose, conn)
            p.active--  // ??? — idle не считается active
        } else {
            alive = append(alive, conn)
        }
    }
    p.idle = alive
    p.mu.Unlock()
    
    // Close вне мьютекса (Close может быть медленным)
    for _, conn := range toClose {
        conn.resource.Close()
    }
}
```

Нюанс: `idle` connections не считаются `active` в нашей модели. `active` = выданные клиенту. `idle` = в пуле. `total = active + len(idle)`. При закрытии idle: total уменьшается, active не меняется.

### IsAlive: цена проверки

`IsAlive()` может быть дорогим: ping к базе (1-10ms), TCP write test. Вызывать при каждом Acquire — overhead. Стратегии:

1. **На Acquire:** проверять только если idle time > threshold (например >1 минуту). Свежие — не проверять.
2. **Background:** health check горутина, периодически. Не задерживает Acquire.
3. **Lazy:** не проверять. Если запрос к ресурсу провалился — Close, retry с новым conn.

Для generic pool: background health check (configurable interval). Для конкретных ресурсов (DB): lazy + retry (database/sql так делает).

## Conn: обёртка с метаданными

### Зачем обёртка

`Resource` — сырой ресурс (TCP conn, DB conn). `Conn` добавляет:
- **Pool reference:** для Release (знает в какой пул вернуться)
- **Timestamps:** createdAt, lastUsed — для lifetime и idle management
- **Use count:** сколько раз переиспользовано
- **Released flag:** защита от двойного Release

### Release vs Close

`Conn.Release()` — "я закончил, верните в пул". Соединение возвращается в idle (если здоровое) или закрывается (если нет).

`Conn.Close()` — "это соединение плохое, уничтожьте". Закрывается принудительно, не возвращается в idle. Когда: ошибка на запросе (`connection reset`), невалидное состояние (transaction left open).

```go
conn, _ := pool.Acquire(ctx)
result, err := conn.Resource().(*DBConn).Query("SELECT ...")
if err != nil {
    conn.Close()  // плохое соединение, не возвращать
    return err
}
conn.Release()  // хорошее, вернуть в пул
```

### Защита от двойного Release

```go
func (c *Conn) Release() {
    if c.released {
        panic("connpool: connection already released")
    }
    c.released = true
    c.pool.release(c)
}
```

Panic — программная ошибка (вызвал Release дважды). Не runtime ситуация. Аналог `sync.Mutex.Unlock` без Lock.

## Stats: наблюдаемость пула

### Метрики

```go
type PoolStats struct {
    Active       int           // выданные клиентам
    Idle         int           // в пуле, ждут
    Total        int           // Active + Idle
    MaxSize      int           // конфигурация
    WaitCount    int64         // суммарно вставших в очередь
    WaitDuration time.Duration // суммарное время ожидания
    CreatedTotal int64         // суммарно создано за всё время
    ClosedTotal  int64         // суммарно закрыто
}
```

`WaitCount` и `WaitDuration` — golden metrics для pool health:
- WaitCount растёт быстро → пул мал для нагрузки
- WaitDuration высокая → клиенты ждут слишком долго
- Idle всегда 0 → пул на пределе
- Idle всегда maxIdle → пул oversized

В production: экспортировать как Prometheus метрики (проект 1 + проект 9).

## Вопросы для самопроверки

1. Factory возвращает ошибку. active-- под мьютексом. notifyWaiters пробуждает ожидающего, тот тоже вызывает factory → тоже ошибка → active-- → notify → каскад. Может ли вся очередь получить ошибки?

2. maxSize=10, maxIdle=5. 10 горутин Acquire, все работают. 10 Release одновременно. 5 в idle, 5 — Close (idle полон). Какие 5 закроются — первые Release или последние?

3. Health check закрыл idle conn. В это же время Acquire берёт из idle (LIFO). Race? Оба под одним мьютексом?

4. Pool с maxSize=1000. Под нагрузкой: все 1000 active. Нагрузка падает. 1000 Release → 1000 idle? Или maxIdle=50 → 950 Close? Стоимость Close 950 соединений?

5. `Conn.Release()` vs `defer conn.Release()`. Почему defer предпочтительнее? Исключение?