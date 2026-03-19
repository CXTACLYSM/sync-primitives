# Модуль 005 — Theory: Token Bucket, lazy refill и резервации

## Алгоритмы rate limiting

### Token Bucket

Метафора: ведро с токенами, ёмкость B (burst). Токены добавляются со скоростью R/сек. Каждый запрос забирает 1 токен. Ведро полное — запросы проходят мгновенно (burst). Ведро пустое — запрос ждёт следующего токена.

Ключевое свойство: burst. После простоя ведро полное — B запросов проходят мгновенно. Это моделирует реальный трафик: пик после тишины, не равномерный поток.

### Leaky Bucket

Метафора: ведро с дыркой. Запросы — вода, наливаемая сверху. Вода вытекает через дырку с фиксированной скоростью R. Если ведро полное — запросы отклоняются (overflow).

Ключевое свойство: сглаживание. Выход всегда ≤ R/сек, независимо от входа. Нет burst. Входной трафик 1000 req/sec при rate 100/sec — 100/sec на выходе, 900 отклонены или в очереди.

### Сравнение

```
Token Bucket:  input ═══▶ [burst OK] ═══▶ output (burst, then rate)
Leaky Bucket:  input ═══▶ [queue/drop] ══▶ output (always rate)
```

Token bucket — для API где burst допустим и желателен (HTTP клиент после idle). Leaky bucket — для систем где равномерность критична (network shaping, audio streaming). Для connection pool — token bucket: burst после idle нагрузки — нормальное поведение.

### Другие алгоритмы

**Fixed Window:** счётчик на временное окно (0-60с: max 100). Проблема: boundary spike — 100 запросов в 59.9с + 100 в 60.1с = 200 за 0.2с.

**Sliding Window Log:** хранить timestamp каждого запроса, считать в скользящем окне. Точно, но memory O(rate).

**Sliding Window Counter:** два fixed window + интерполяция. Компромисс точности и памяти.

Token bucket — стандарт для Go rate limiting. `golang.org/x/time/rate` использует token bucket.

## Lazy refill: вычисление вместо тикера

### Принцип

Вместо горутины-тикера, добавляющей токены каждый tick, — вычислять текущее количество при каждом обращении:

```go
func (l *RateLimiter) refill() {
    now := time.Now()
    elapsed := now.Sub(l.lastTime)
    l.tokens += l.rate * elapsed.Seconds()
    if l.tokens > float64(l.burst) {
        l.tokens = float64(l.burst)
    }
    l.lastTime = now
}
```

При каждом `Allow()`, `Wait()`, `Reserve()` — сначала `refill()`, потом проверка.

### Почему lazy лучше тикера

**Точность.** Тикер с интервалом 1ms (rate=1000/sec) — добавляет 1 токен каждую ms. Но `time.Ticker` не гарантирует точность <1ms (OS scheduling jitter). Lazy: `elapsed = 1.234ms` → `tokens += 1000 * 0.001234 = 1.234` — математически точно.

**Эффективность.** Тикер — фоновая горутина, пробуждается каждый tick даже без запросов. 100 limiters × ticker = 100 горутин. Lazy: ноль горутин, вычисление O(1) при обращении. Без обращений — zero CPU.

**Простота управления.** `SetRate(newRate)`: lazy — поменять `l.rate`, следующий refill использует новое значение. Тикер — остановить старый ticker, создать новый с новым интервалом.

**Отсутствие утечек.** Тикер без `Stop()` — горутина живёт вечно. Lazy — нет горутин, нечему утекать.

### float64 для токенов

Токены — дробные. Rate=10/sec, прошло 50ms: `10 * 0.05 = 0.5 токена`. С int: 0 (округление). Следующие 50ms: ещё 0. За 100ms: вместо 1 токена — 0 (два округления). С float: `0.5 + 0.5 = 1.0` — корректно.

Потребление — целое (1 запрос = 1 токен). Но накопление — дробное. `tokens >= 1.0` → allow. `tokens = 0.9999` → deny. Гранулярность ограничена точностью float64 (~15 значащих цифр) и точностью `time.Now()` (~1ns). Для rate до 10^9/sec — float64 достаточен.

## Allow: неблокирующий check-and-consume

### Алгоритм

```go
func (l *RateLimiter) Allow() bool {
    return l.AllowN(1)
}

func (l *RateLimiter) AllowN(n int) bool {
    l.mu.Lock()
    defer l.mu.Unlock()
    
    l.refillLocked()
    
    if l.tokens >= float64(n) {
        l.tokens -= float64(n)
        return true
    }
    return false
}
```

Под мьютексом: refill → check → consume. Атомарно: никто не может забрать токены между check и consume. Не модифицирует tokens при отказе (в отличие от Reserve).

### Когда использовать

`Allow()` — для fire-and-forget throttling:

```go
if !limiter.Allow() {
    http.Error(w, "rate limit exceeded", 429)
    return
}
// обработать запрос
```

Не ждать, не ставить в очередь. Превысил лимит — немедленный 429 Too Many Requests. Для API с strict SLA: лучше отклонить чем задержать.

## Wait: блокирующий acquire

### Алгоритм

```go
func (l *RateLimiter) Wait(ctx context.Context) error {
    return l.WaitN(ctx, 1)
}

func (l *RateLimiter) WaitN(ctx context.Context, n int) error {
    l.mu.Lock()
    l.refillLocked()
    
    if l.tokens >= float64(n) {
        l.tokens -= float64(n)
        l.mu.Unlock()
        return nil
    }
    
    // Вычислить время ожидания
    deficit := float64(n) - l.tokens
    delay := time.Duration(deficit / l.rate * float64(time.Second))
    l.tokens -= float64(n)  // "занять из будущего" (tokens может стать отрицательным)
    l.mu.Unlock()
    
    // Ждать
    timer := time.NewTimer(delay)
    defer timer.Stop()
    
    select {
    case <-timer.C:
        return nil
    case <-ctx.Done():
        // Вернуть зарезервированные токены
        l.mu.Lock()
        l.tokens += float64(n)
        l.mu.Unlock()
        return ctx.Err()
    case <-l.done:
        l.mu.Lock()
        l.tokens += float64(n)
        l.mu.Unlock()
        return ErrLimiterClosed
    }
}
```

### Отрицательные токены

`Wait` уменьшает tokens сразу, даже если их недостаточно. `tokens` может стать отрицательным: "забронировано из будущего". Следующий вызов Wait/Allow видит отрицательные tokens → ждёт дольше (deficit больше).

Зачем не ждать и потом уменьшить? Race: между вычислением delay и пробуждением — другая горутина могла сделать Wait и получить те же токены. С отрицательными: каждый Wait "бронирует" свои токены немедленно. Следующий Wait видит реальный deficit. Нет double-spend.

### Cancel: возврат токенов

При отмене контекста — `tokens += n` возвращает зарезервированные токены. Следующий вызов увидит больше tokens, задержка меньше. Это отмена резервации: "я забронировал но не использую".

## Reserve: информированное решение

### Зачем Reserve отдельно от Wait

`Allow` — binary: да/нет. `Wait` — блокирующий: ждать. `Reserve` — информационный: "если хочешь использовать токен, подожди delay". Вызывающий код решает:

```go
r := limiter.Reserve()
if r.Delay() > 500*time.Millisecond {
    r.Cancel()  // слишком долго, не ждать
    return fallback()
}
time.Sleep(r.Delay())
proceed()
```

Паттерн: adaptive throttling. Если delay маленький — ждать. Если большой — fallback (кэш, degraded response, redirect).

### Reservation как объект

```go
type Reservation struct {
    ok      bool
    tokens  int
    delay   time.Duration
    limiter *RateLimiter
}
```

`Cancel()` — вернуть токены. Без Cancel — токены потрачены даже если операция не выполнена. Утечка capacity.

`OK()` — может быть false если limiter closed или rate=Inf (no limit).

## Динамическое изменение rate

### SetRate: adaptive throttling

```go
func (l *RateLimiter) SetRate(rate float64) {
    l.mu.Lock()
    defer l.mu.Unlock()
    l.refillLocked()  // зафиксировать текущие tokens по старому rate
    l.rate = rate
}
```

`refillLocked()` перед изменением: вычислить tokens по старому rate за прошедшее время. Потом — новый rate для будущих вычислений. Без refill: если rate увеличивается — следующий refill с новым rate за всё прошедшее время → скачок tokens. С refill: плавный переход.

### Production сценарии

**Auto-scaling:** мониторинг показывает low error rate → `SetRate(currentRate * 1.5)`. High error rate → `SetRate(currentRate * 0.5)`. Adaptive: система подстраивается под текущие возможности backend.

**Emergency stop:** `SetRate(0)` — остановить все запросы. `SetRate(prevRate)` — возобновить. Rate=0 → tokens не пополняются, burst быстро исчерпывается, все запросы отклоняются.

**Gradual ramp-up:** после деплоя — `SetRate(10)`, через 5 минут `SetRate(50)`, через 15 минут `SetRate(100)`. Canary deployment: медленное увеличение трафика.

## time.Timer: правильное использование

### Timer vs time.After

```go
// Timer — reusable, stoppable
timer := time.NewTimer(delay)
defer timer.Stop()
select {
case <-timer.C:
case <-ctx.Done():
}

// time.After — создаёт timer, нет способа остановить
select {
case <-time.After(delay):  // leak до Go 1.23!
case <-ctx.Done():
}
```

`time.After` в select — утечка timer до Go 1.23: если ctx.Done() сработал первым, timer продолжает жить до срабатывания. Для Wait с частыми cancel — тысячи живых timers.

`time.NewTimer` + `defer Stop()` — корректно: timer останавливается при отмене. До Go 1.23 после Stop нужно drain channel: `if !timer.Stop() { <-timer.C }`. С Go 1.23 — Stop достаточен.

### Timer точность

`time.NewTimer(100*time.Millisecond)` — пробуждение через ~100ms ± jitter. OS scheduling: горутина паркуется, runtime timer добавляется в heap. При срабатывании — горутина ставится в run queue. Scheduling delay: 0-10µs (нормальная нагрузка), 0-1ms (высокая нагрузка). Для rate limiter — microsecond jitter несущественен.

## Вопросы для самопроверки

1. Token bucket: rate=100/sec, burst=10. Запросы приходят со скоростью 200/sec. Через сколько секунд ведро опустеет? Какой steady-state throughput?

2. Lazy refill под мьютексом: `refill()` вызывает `time.Now()` — syscall. Под мьютексом syscall — держим lock дольше. Как минимизировать время под lock?

3. `Wait(ctx)` делает `tokens -= n` сразу (может стать отрицательным) и потом ждёт delay. Другая горутина вызывает `Allow()` — видит отрицательные tokens → false. Корректно ли что Allow denied из-за Wait другой горутины?

4. Rate=0, burst=0. `Allow()` → ? `Wait(ctx)` → ? Является ли это валидной конфигурацией?

5. `SetRate(newRate)` вызывается из одной горутины, `Allow()` — из другой. Между refill и установкой нового rate — окно? Или мьютекс гарантирует атомарность?