# Модуль 005 — QA: Token Bucket Rate Limiter

## Блок 1: Алгоритм и математика

**Q1.1:** Rate=100/sec, burst=20. Limiter idle 5 секунд. Сколько токенов в ведре? Приходит 50 запросов мгновенно. Сколько пройдёт? Через сколько ms пройдёт 51-й?

**A1.1:** После 5 секунд idle: `tokens = min(0 + 100*5, 20) = 20` (capped at burst). 50 запросов мгновенно: первые 20 пройдут (burst). 21-й ждёт: tokens=0, need 1, rate=100/sec → delay = 1/100 = 10ms. 22-й: 20ms. ... 50-й: 300ms. 51-й: 310ms после первого запроса. Суммарно: 20 мгновенно + 30 за 300ms = 50 за 300ms. Effective burst rate: 50/0.3 = ~167 req/sec. Steady state: 100 req/sec.

**Q1.2:** Rate=10/sec, burst=1. Запросы каждые 80ms (12.5/sec > rate). Паттерн Allow: true, false, true, false, ... Объяснить: почему чередование? Какой effective throughput?

**A1.2:** Burst=1: максимум 1 токен. Rate=10/sec: 1 токен каждые 100ms. Запрос каждые 80ms. Первый: tokens=1 → true, tokens=0. Через 80ms: tokens=10*0.08=0.8 < 1 → false. Через ещё 80ms (160ms от начала): tokens=0.8+0.8=1.6, cap=1 → tokens=1 → true, tokens=0. Через 80ms: 0.8 → false. Чередование true/false. Effective: 1 из 2 пропускается → 12.5/2 = 6.25 req/sec. Меньше rate (10/sec) потому что burst=1 и запросы не выровнены с refill.

**Q1.3:** Два rate limiter последовательно: `limiter1(rate=100, burst=10)` → `limiter2(rate=50, burst=5)`. Запрос проходит оба. Effective rate? Effective burst?

**A1.3:** Effective rate = min(100, 50) = 50/sec (bottleneck). Effective burst: limiter1 пропустит 10 мгновенно, limiter2 пропустит 5 из них, остальные 5 ждут. Effective burst ≈ 5 (определяется меньшим burst). Последовательные limiters: effective rate = min, effective burst ≈ min. Для connection pool: rate limiter перед семафором — effective throughput = min(rate limit, semaphore throughput).

---

## Блок 2: Lazy refill

**Q2.1:** Lazy refill: `tokens += rate * elapsed.Seconds()`. `time.Now()` вызывается под мьютексом. На Linux `time.Now()` — `clock_gettime(CLOCK_REALTIME)` — ~20-50ns. Под мьютексом это overhead. Как минимизировать?

**A2.1:** Вызвать `time.Now()` ДО Lock:
```go
func (l *RateLimiter) Allow() bool {
    now := time.Now()  // вне мьютекса
    l.mu.Lock()
    defer l.mu.Unlock()
    l.refillAt(now)  // передать now как параметр
    // ...
}
```
`time.Now()` вне lock — не критическая секция. Разница между `now` и реальным моментом проверки — наносекунды (время захвата мьютекса). Для rate limiter — несущественная погрешность. Сокращает время под мьютексом на ~30-50ns.

**Q2.2:** `time.Now()` использует monotonic clock (с Go 1.9). `elapsed := now.Sub(lastTime)` — всегда ≥ 0, даже при NTP adjustment. Что произошло бы с rate limiter если elapsed < 0 (clock went backwards)?

**A2.2:** `tokens += rate * negative_elapsed` → tokens уменьшаются. Rate limiter "забирает" токены при отрицательном elapsed. Запросы отклоняются без причины. С monotonic clock: невозможно — `time.Duration` от monotonic Sub всегда ≥ 0. Go автоматически использует monotonic для `Sub` между двумя `time.Now()`. Но: если `lastTime` десериализовано из файла/базы (wall clock) — monotonic часть потеряна, `Sub` использует wall clock → NTP adjustment может дать отрицательный elapsed. Правило: rate limiter state (lastTime) — только in-memory monotonic, не persist.

**Q2.3:** Lazy refill не вызывается если нет запросов. Limiter idle 24 часа. Первый запрос: `tokens += rate * 86400`. Rate=100: tokens=8,640,000. Clamp до burst=10: tokens=10. Что если burst=10,000,000? Tokens=8,640,000. 8.6M мгновенных запросов — это burst?

**A2.3:** Формально — да, burst=10M позволяет 8.6M мгновенных запросов. Практически — абсурд. Burst должен отражать реальный допустимый всплеск: "после idle, сколько запросов можно отправить мгновенно без перегрузки backend". Для DB connection pool: burst = max connections (10-50). Для HTTP API: burst = 2-3x average request rate per second. Burst=10M — configuration error. Validation в конструкторе: `if burst > reasonableMax { warn }`.

---

## Блок 3: Wait и конкурентность

**Q3.1:** `Wait(ctx)` делает `tokens -= 1` (tokens может стать -5), вычисляет delay, отпускает мьютекс, ждёт timer. В это время 10 горутин вызывают `Allow()`. Все видят tokens < 0 → все false. Но только одна горутина "заняла" эти токены. Остальные "наказаны" за чужую Wait. Справедливо ли это?

**A3.1:** Справедливо с точки зрения rate: tokens отрицательные означают "на ближайшие N секунд bandwidth занят". Allow видит реальную картину: throughput уже забронирован Wait-горутинами. Позволить Allow при отрицательных tokens → превысить rate (Wait-горутины используют свои токены через delay, Allow-горутины — немедленно). Unfair с точки зрения UX: Allow-горутина не знает почему denied — потому что rate exceeded или потому что Wait занял capacity. Решение: `Reservation` с `Delay()` — вызывающий код видит сколько ждать и решает сам.

**Q3.2:** Три горутины одновременно вызывают `Wait(ctx)`. Rate=1/sec, burst=0 tokens. Delay для каждой: G1=1s, G2=2s, G3=3s. G2 cancel через 1.5s. `tokens += 1` возвращается. G3 delay пересчитается?

**A3.2:** Нет. G3 уже создала timer на 3s и ждёт. Cancel G2 вернул 1 токен: tokens=-2 → -1. Но G3 не перепроверяет — она спит на timer. G3 проснётся через 3s, но tokens к тому моменту: -1 + 1*3 = 2 (refill 3 seconds). Реально: -1 + 3*1 = 2 tokens. G3 забронировала 1, осталось 1. Всё корректно: G3 не проснётся раньше, но tokens к моменту пробуждения будут достаточны (refill за время delay). Cancel G2 "ускорит" следующий Allow/Wait после пробуждения G3, но G3 саму не ускорит.

**Q3.3:** `Wait(ctx)` — ctx отменён ПОСЛЕ пробуждения timer (race). Timer fired → горутина в runnable queue → ещё не выполняется → ctx cancelled. `select` видит обе ветки ready. Может выбрать `ctx.Done()`. Токены были зарезервированы, теперь возвращаются. Потеря throughput?

**A3.3:** Да — marginal потеря. Горутина "подождала" delay, но при пробуждении `select` выбрал cancel. Токены возвращены — следующий запрос получит их. Но текущий запрос потратил delay время и не выполнен. Это acceptable race: вероятность мала (cancel и timer в наносекундном окне), последствие — один запрос ненужно задержан. Для strict ordering: проверить `ctx.Err()` после `<-timer.C` ветки — если cancel, вернуть ошибку. Но обычно: timer fired = "ваше время пришло" → proceed regardless of context.

---

## Блок 4: Reserve и отрицательные токены

**Q4.1:** Reserve не блокируется — всегда возвращает Reservation. Tokens может стать -100. Следующий Allow: `refill()` добавляет tokens, но если deficit=-100 и rate=10/sec — нужно 10 секунд чтобы tokens стали ≥ 0. Все Allow и Wait за эти 10 секунд — denied/long delay. Как предотвратить unbounded negative?

**A4.1:** Ограничить: `Reserve` проверяет `if l.tokens - float64(n) < -float64(l.burst) { return Reservation{ok: false} }`. Tokens не опускаются ниже `-burst`. Максимальный "кредит из будущего" = burst. Delay ≤ burst/rate секунд. Альтернатива: `Reserve` возвращает OK=false если delay > maxWait (configurable). `golang.org/x/time/rate`: не ограничивает negative, но `Reserve()` возвращает delay — вызывающий код решает ждать ли. Ответственность на caller.

**Q4.2:** `Reservation.Cancel()` — вернуть токены. Код:
```go
r := limiter.Reserve()
// ... прошло 500ms ...
r.Cancel()
```
500ms прошло — часть токенов уже "refilled" за это время. Cancel возвращает полное количество tokens? Или скорректированное за elapsed?

**A4.2:** Зависит от реализации. Simple: `tokens += r.tokens` — возвращает полное количество. Tokens может стать больше чем было до Reserve. `x/time/rate` подход: скорректировать за elapsed. Если прошло 500ms с rate=10/sec — 5 токенов уже refilled. Вернуть `max(0, r.tokens - refilled_since_reservation)`. Если delay был 300ms, а прошло 500ms — все токены уже бы refilled → Cancel returns 0. Корректированный подход точнее, но сложнее. Simple подход: overcompensation — следующие запросы получат бонус. Для production: корректированный.

**Q4.3:** `Reserve()` + `Cancel()` как probe: "сколько ждать?".
```go
r := limiter.Reserve()
delay := r.Delay()
r.Cancel()  // не использовать, только узнать delay
```
Проблема: Reserve уменьшил tokens, Cancel вернул. Между Reserve и Cancel — другой вызов может увидеть сниженные tokens. Как сделать probe без side effect?

**A4.3:** Добавить метод `EstimateDelay(n int) time.Duration`:
```go
func (l *RateLimiter) EstimateDelay(n int) time.Duration {
    l.mu.Lock()
    defer l.mu.Unlock()
    l.refillLocked()
    if l.tokens >= float64(n) {
        return 0
    }
    deficit := float64(n) - l.tokens
    return time.Duration(deficit / l.rate * float64(time.Second))
}
```
Read-only — не модифицирует tokens. Но: estimate может устареть (TOCTOU). Между estimate и actual request — другие горутины могут изменить tokens. Estimate — hint, не guarantee.

---

## Блок 5: Сравнения

**Q5.1:** `golang.org/x/time/rate.Limiter` — API: `Allow()`, `Wait(ctx)`, `Reserve()`, `AllowN(time.Time, int)`, `WaitN(ctx, int)`, `ReserveN(time.Time, int)`. Наша реализация vs x/time/rate: основные отличия?

**A5.1:** (1) `x/time/rate` принимает `time.Time` в AllowN/ReserveN — для тестирования с fake time. Наша — `time.Now()` внутри. (2) `x/time/rate` не имеет `Close()` — lifetime managed externally. (3) `x/time/rate` не имеет `SetRate`/`SetBurst` — immutable after creation (в Go 1.x; в более новых — есть `SetLimit`, `SetBurst`). (4) `x/time/rate.Limit` — special type (float64), `rate.Inf` — infinite rate (no limiting). (5) `x/time/rate` — battle-tested, наша — educational. Для production: `x/time/rate`. Для understanding internals: своя.

**Q5.2:** Rate limiter vs semaphore. Rate=100/sec, semaphore=10, request duration=100ms. Throughput ограничен чем? Посчитать effective throughput для: (a) только rate limiter, (b) только semaphore, (c) оба.

**A5.2:** (a) Rate limiter only: 100/sec (by definition). (b) Semaphore only: 10 concurrent × 1000ms/100ms = 100/sec. (c) Both: min(100, 100) = 100/sec. Совпадение. Но: если request duration = 200ms: semaphore throughput = 10/0.2 = 50/sec. Both: min(100, 50) = 50/sec (semaphore bottleneck). Если request duration = 50ms: semaphore = 10/0.05 = 200/sec. Both: min(100, 200) = 100/sec (rate limiter bottleneck). Rate limiter и semaphore — complementary controls.

**Q5.3:** Token bucket rate limiter для per-IP limiting. 1000 unique IPs. Один global limiter vs 1000 per-IP limiters. Memory? Overhead?

**A5.3:** Global: 1 RateLimiter (~100 bytes). All IPs share limit — one heavy user blocks all. Per-IP: 1000 × ~100 bytes = ~100KB. Each IP independent. Memory OK. Overhead: map[IP]*RateLimiter — lookup per request (SafeMap from module 004). Cleanup: IPs that haven't been seen for N minutes — garbage. Without cleanup: map grows unbounded (DoS: attacker sends from random IPs → unbounded memory). Solution: per-IP limiter with TTL, eviction goroutine, or `sync.Map` with periodic purge. For connection pool: per-client limiter pattern.

---

## Блок 6: Production сценарии

**Q6.1:** Rate limiter в HTTP middleware. Client sends 1000 req/sec, rate=100/sec. 900 запросов → 429. Client retries immediately. New 900 retries + 1000 new = 1900 req/sec. Rate limiter handles 100, rejects 1800. Retry storm. Как предотвратить?

**A6.1:** (1) `Retry-After` header: 429 response с `Retry-After: 1` — клиент ждёт 1 секунду. Well-behaved клиенты слушают. (2) Exponential backoff: клиент увеличивает delay между retries (1s, 2s, 4s, 8s). (3) Rate limit retries: отдельный rate limiter для retried requests (penalty). (4) Server-side: jitter в Retry-After (1-3s random) — retries не синхронизируются. (5) Circuit breaker: после N consecutive 429 — клиент перестаёт пытаться на M секунд. Правило: rate limiter без retry policy — бесполезен. Клиент retry без backoff — DDoS himself.

**Q6.2:** Rate=0, burst=0 — "emergency stop". `SetRate(0)` в runtime. Проблема: горутины в `Wait(ctx)` — delay = 1/0 = Inf. `time.Duration(math.Inf(1))` — overflow. Как обработать rate=0 в Wait?

**A6.2:** Специальный случай:
```go
if l.rate == 0 {
    // Tokens никогда не добавятся. Wait бесконечен.
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-l.done:
        return ErrLimiterClosed
    }
}
```
Rate=0: не вычислять delay (division by zero), не создавать timer. Просто ждать cancel или close. Allow при rate=0: после exhaustion burst — всегда false. `x/time/rate`: `rate.Limit(0)` → `Wait` блокируется до cancel. `Allow` → false после burst.

**Q6.3:** Distributed rate limiting: 5 серверов, каждый с rate=100/sec. Суммарный rate: 500/sec? Или каждый клиент получает 100/sec на каждом сервере (500/sec total)?

**A6.3:** Зависит от архитектуры. Local rate limiter на каждом сервере: клиент с round-robin load balancer → 100/sec × 5 = 500/sec total. Не то что хотели. Решения: (1) rate/N на каждом сервере: 100/5 = 20/sec per server, total 100/sec. Но: если 1 server down → 80/sec (не 100). (2) Centralized: Redis-based rate limiter (lua script для atomic check-and-decrement). Все серверы проверяют один counter. (3) Sliding window в Redis: `INCR key; EXPIRE key 1`. (4) Token bucket в Redis: lua script для refill + consume. Trade-off: local = fast (no network), imprecise (per-server). Central = precise, slow (Redis RTT per request). Hybrid: local burst, periodic sync with central counter.