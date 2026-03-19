# Модуль 008 — QA: Bulkhead Pattern

## Блок 1: Изоляция и порядок acquire

**Q1.1:** Acquire: partition sem → pool. Горутина A ("critical"): sem acquired → pool.Acquire (ждёт, pool полон). Горутина B ("background"): sem acquired → pool.Acquire (ждёт). Pool full потому что "normal" partition заняла все conn. Ни A ни B не могут получить conn. "critical" заблокирован из-за "normal". Bulkhead не изолировал?

**A1.1:** Верно — pool стал bottleneck. Bulkhead изолирует partition-level concurrency, но pool — shared. Если sum(partition active) = pool maxSize → новые acquire в ЛЮБОЙ partition блокируются на pool. Bulkhead гарантирует: partition не превысит свой лимит. Не гарантирует: partition получит conn если pool исчерпан другими. Решение: sum(partition limits) ≤ pool maxSize — hard guarantee. Или: per-partition pools (полная изоляция, но больше ресурсов). Или: accept risk с мониторингом.

**Q1.2:** Acquire: partition sem → pool. Release: pool → partition sem. Почему не Release: partition sem → pool? Что конкретно сломается?

**A1.2:** Release partition sem → pool. Сценарий: Release partition sem (slot свободен). Другая горутина C: partition sem acquired (slot взят). C → pool.Acquire. Но conn ещё не вернулся в pool (pool.Release не вызван). C может получить другой idle conn — OK. Или C блокируется на pool (нет idle) — ждёт пока первая горутина вызовет pool.Release. Не deadlock, но suboptimal: C ждёт зря. Pool Release первым: conn в idle → partition sem Release → C gets partition slot → pool.Acquire → находит idle conn мгновенно. Оптимальнее.

Deadlock сценарий при определённых условиях: если pool.Release и partition.Release держат какие-то внутренние locks в пересекающемся порядке — potential deadlock. С текущей архитектурой (pool и partition — независимые мьютексы, не nested) — deadlock невозможен. Но: pool → partition Release — conventional и safer.

**Q1.3:** TryAcquire("critical"): partition sem.TryAcquire → true → pool.TryAcquire → false (pool full). Partition sem acquired, но conn не получен. Что делать?

**A1.3:** Partition sem.Release() — вернуть slot. Return false. Обязательно: если pool TryAcquire failed — partition slot MUST be released. Иначе: partition slot утечёт. Код:
```go
func (b *Bulkhead) TryAcquire(partition string) (*BulkheadConn, bool) {
    p := b.getOrCreatePartition(partition)
    if !p.sem.TryAcquire() {
        p.rejected.Add(1)
        return nil, false
    }
    conn, ok := b.pool.TryAcquire()
    if !ok {
        p.sem.Release()  // ВАЖНО: вернуть partition slot
        return nil, false
    }
    p.active.Add(1)
    return &BulkheadConn{conn: conn, partition: p, bulkhead: b}, true
}
```

---

## Блок 2: Oversubscription

**Q2.1:** Три partition: A(10), B(10), C(10). Pool maxSize=15. Steady state: A=5, B=5, C=5 (total=15=maxSize). A получает burst: 5 extra → A=10, B=5, C=5, total=20>15. Пять запросов A блокируются на pool. B Release 2: pool slots free → 2 из ожидающих A получают conn. Корректно?

**A2.1:** Корректно. Pool — FIFO waiter queue (модуль 006). Ожидающие A попадают в pool waiter queue. B Release → pool Release → первый waiter (может быть A) получает conn. Pool не знает о partitions — обслуживает FIFO. A "забирает" conn освобождённые B. Это oversubscription в действии: при пике A "заимствует" capacity у B (B пользуется меньше лимита). Проблема: если B тоже на пике → обе ждут, total limited by pool maxSize=15, не 20.

**Q2.2:** Oversubscription ratio: sum(partition limits) / pool maxSize. Ratio=3.0 (30 / 10). Safe? Ratio=1.5 (15 / 10)? Ratio=1.0 (10 / 10)?

**A2.2:** Ratio=1.0: no oversubscription, guaranteed isolation. Каждая partition получит свой лимит всегда. Ratio=1.5: mild oversubscription. Работает если peak overlap < 50%. При simultaneous peaks: 50% запросов сверх pool blocked. Ratio=3.0: aggressive. Работает только если partitions rarely overlap. При double-peak: 2/3 запросов blocked. Правило: ratio > 2.0 — red flag, нужен explicit justification (disjoint schedules, probabilistic model). Production: ratio 1.0–1.5 для critical systems, 1.5–2.0 для best-effort.

**Q2.3:** Мониторинг oversubscription. Метрика: `pool_active / pool_max_size`. Порог 0.9. Но: pool_active=9/10=0.9, одна partition использует 9, другие по 0. Это не oversubscription — это one partition dominating. Как различить oversubscription от single-partition dominance?

**A2.3:** Per-partition метрики: `partition_active / partition_max`. Oversubscription: несколько partitions near max одновременно. Dominance: одна partition near max, остальные low. Dashboard: все partition_active на одном графике. Oversubscription = несколько линий near max + pool_active near maxSize. Dominance = одна линия high. Alert: `count(partition_active / partition_max > 0.8) > 1 AND pool_active > 0.9 * pool_max` — oversubscription detected.

---

## Блок 3: Partition-aware conn

**Q3.1:** Acquire возвращает `*BulkheadConn` (wrapper). Вызывающий код: `bc.Resource().(*net.TCPConn).Write(data)`. Два уровня indirection: BulkheadConn → Conn → Resource. Performance overhead?

**A3.1:** BulkheadConn → Conn: pointer dereference (~1ns). Conn → Resource: interface method call (~5ns, через itab). Суммарно: ~6ns per resource access. Для IO операции (Write: ~1µs network) — 0.6% overhead. Negligible. Для CPU-bound горячего пути (millions of calls/sec) — may be measurable. Но: bulkhead используется для IO-bound operations (DB, network) — overhead invisible.

**Q3.2:** `BulkheadConn.Release()` — idempotent (released bool check). Горутина A и B обе имеют ссылку на один BulkheadConn (баг: conn shared между горутинами). A.Release(). B.Release(). Что произойдёт?

**A3.2:** A.Release(): released=false → released=true, conn.Release(), partition.Release(). B.Release(): released=true → return (no-op). Partition sem и pool Release — однократно. Корректно с точки зрения ресурсов, но: sharing BulkheadConn — программная ошибка. Conn предназначен для одного владельца. Idempotent Release — защита от double-release, не от sharing. Data race: `bc.released` read+write из двух горутин без sync → race detector поймает. Решение: `released` через `atomic.Bool` или accept panic (double-release = bug).

**Q3.3:** Acquire("critical") → BulkheadConn. Код: `defer bc.Release()`. Между Acquire и defer — паника. BulkheadConn не released. Partition slot leaked. Как паттерн из модуля 006 (defer сразу после acquire) применяется здесь?

**A3.3:** Аналогично модулю 006:
```go
bc, err := bulkhead.Acquire(ctx, "critical")
if err != nil {
    return err
}
defer bc.Release()  // СРАЗУ после acquire, ДО любого кода который может паниковать
```
Defer зарегистрирован до паники → выполнится при раскрутке стека. Partition slot и conn — Released. Паттерн идентичен файлам (defer Close), мьютексам (defer Unlock), семафорам (defer Release). Единственная надёжная гарантия cleanup.

---

## Блок 4: Dynamic resize и fallback

**Q4.1:** ResizePartition("critical", 5→8). Три новых слота. Но: pool maxSize=10, текущий pool active=10 (pool полон). Три новых "critical" горутины: partition sem acquired (slots 6,7,8) → pool.Acquire → blocked (pool full). Partition limit увеличен, но pool не вырос. Resize бесполезен?

**A4.1:** Бесполезен в данный момент — pool bottleneck. Resize полезен когда pool НЕ на пике: увеличенная partition "critical" получит больше conn при нормальной нагрузке. При пике pool — resize не помогает. Правильный resize: partition + pool одновременно. Или: resize когда pool utilization < 70%.

**Q4.2:** ResizePartition("critical", 5→3). Active=4. Новый семафор capacity=3. Как 4 active горутины освободят слоты? Они Release на старом семафоре или на новом?

**A4.2:** При resize: старый семафор Close → ожидающие получают error. Active горутины имеют `BulkheadConn` с ссылкой на partition. При Release: partition.Release() → sem.Release(). Если sem заменён на новый — Release на старом? Или на новом? Зависит от реализации. Вариант A: Release всегда на текущем sem (partition.sem). Active горутины Release → новый sem.Release → sem может получить больше Release чем Acquire (disbalance). Вариант B: BulkheadConn хранит ссылку на sem который был при Acquire. Release → тот же sem. Старый sem: 4 Release уменьшат count. Новый sem: начинает "чистым". Вариант B корректнее, но сложнее (каждый BulkheadConn хранит sem pointer).

Практичный подход: resize ждёт drain active до 0 (или ≤ newMax), потом замена. Resize — rare operation, допустимо подождать.

**Q4.3:** Fallback chain: "critical" → "overflow" → error. "critical" full, TryAcquire("critical") → false. Try "overflow" → true → pool.Acquire → conn. При Release: Release в "overflow" partition, не "critical". Метрики "critical" не отражают что запрос обслужен (через overflow). Как трекать?

**A4.3:** Метрика `fallback_total{primary="critical", fallback="overflow"}` — счётчик fallback использований. Stats показывает: critical rejected + overflow served = total critical demand. Dashboard: critical.rejected + overflow.total ≈ critical demand exceeding capacity. Alert: overflow.active > 0 → critical overloaded, consider resize. BulkheadConn: хранить и primary и actual partition — для метрик.

---

## Блок 5: Production сценарии

**Q5.1:** E-commerce: "checkout" (critical, 5 conn), "search" (normal, 10 conn), "recommendations" (background, 3 conn). Black Friday: search burst 100x. Без bulkhead: search exhausts all 15 conn, checkout fails, revenue loss. С bulkhead: search blocked at 10, checkout continues. Quantify: revenue saved = checkout TPS × average order value × downtime prevented.

**A5.1:** Без bulkhead: checkout down 10 минут при search storm. checkout TPS=50, AOV=$100. Revenue loss: 50 × $100 × 600sec = $3,000,000. С bulkhead: checkout continues, zero downtime. Search degraded (10 conn vs desired 50) — slower results but available. Revenue: $0 loss from checkout. Search revenue impact: minimal (users still buy, just slower search). Bulkhead ROI: $3M saved / hours of implementation. Simplified calculation, but order of magnitude correct for large e-commerce.

**Q5.2:** Partition "batch" maxConns=3. Batch job needs 100 sequential DB queries. Each query: acquire conn → query 10ms → release. 100 × (acquire + 10ms + release). Partition limit 3 — но sequential: only 1 conn at a time. Partition limit irrelevant? When does batch partition limit matter?

**A5.2:** Sequential batch: 1 conn at a time. Partition limit 3 not reached. Limit matters when: (1) Parallel batch: batch job uses 3 goroutines, each with conn. (2) Multiple batch jobs: 3 simultaneous batch jobs, each 1 conn. (3) Burst: batch acquires conn for longer operations (bulk insert 500ms). Partition limit = concurrent batch connections, not total batch queries. For sequential single-job: limit irrelevant except as ceiling. Set based on expected parallelism of batch workload.

**Q5.3:** Partition для downstream service: "payments-api" maxConns=5. Payments API down (timeout 30s per request). 5 conns busy (timeout). New requests: partition full, rejected immediately. This is good — fast failure instead of 30s timeout for every request. But: what if payments recovers? 5 timeout conns release → 5 slots free → burst of 5 → payments may fail again (not fully recovered). How to handle recovery?

**A5.3:** Gradual recovery: после timeout storm — don't immediately use all 5 slots. Approaches: (1) Circuit breaker (next module concept): after N timeouts → open circuit → reject without trying → after cooldown → half-open → allow 1 request → if success → close circuit. (2) Slow ramp: after recovery, temporarily reduce partition to 1 → success → 2 → success → 3... Exponential increase. (3) Health check: before Acquire → ping payments API. If ping fails → reject. (4) Rate limit partition recovery: rate limiter on "payments-api" partition, start at 1 req/sec, increase gradually.

---

## Блок 6: Дизайн и trade-offs

**Q6.1:** Per-partition pools (полная изоляция: каждая partition = свой Pool) vs shared pool + partition semaphores (текущий дизайн). Trade-offs?

**A6.1:** Per-partition pools: (+) полная изоляция — partition A failure не влияет на B. (+) Независимый lifecycle (health check, maxLifetime per partition). (-) Больше connections: 5+10+3=18 vs shared pool 15. (-) Нет sharing: "critical" idle conns не переиспользуются "normal". (-) Больше factory calls (больше TCP connects). Shared pool + semaphores: (+) Меньше total connections (shared idle). (+) Efficient reuse. (-) Pool becomes bottleneck при oversubscription. (-) Failure isolation limited (pool-level failure affects all). Выбор: per-partition для high-isolation requirements (payments vs analytics). Shared для efficient resource usage (homogeneous backends).

**Q6.2:** Bulkhead granularity: per-endpoint ("/api/v1/checkout"), per-service ("payments"), per-tenant ("customer-A"), per-priority ("high/medium/low"). When each?

**A6.2:** Per-endpoint: fine-grained, many partitions. For: different endpoints have different resource needs (checkout=5, search=10). Against: too many partitions, management overhead. Per-service: coarse. For: isolate upstream services (payments API vs shipping API). Standard for microservices. Per-tenant: multi-tenant SaaS. For: one tenant's load doesn't affect others. Against: potentially thousands of partitions (one per tenant). Per-priority: simplest. For: high/medium/low priority with clear SLA. Against: coarse, doesn't differentiate within same priority. Common pattern: two levels — per-service (outer) + per-priority (inner). Each service gets its pool, within pool — priority partitions.

**Q6.3:** Bulkhead добавляет complexity: partition configuration, monitoring per-partition, fallback logic, resize. Simpler alternative: just use smaller pool maxSize per-service (deploy separate service instances). When is bulkhead in-process justified vs separate deployment?

**A6.3:** In-process bulkhead: (+) Zero network overhead. (+) Dynamic resize without redeploy. (+) Shared underlying resources (efficient). (-) Shared failure domain (process crash affects all). (-) Complexity in code. Separate deployment: (+) Full isolation (different processes/machines). (+) Independent scaling. (+) Independent failure domains. (-) Network overhead. (-) More infrastructure. Justified in-process when: (1) All categories access same backend (one DB, different query patterns). (2) Low overhead critical (sub-millisecond routing). (3) Dynamic categorization (runtime decision which partition). Separate when: (1) Different backends (payments service vs email service). (2) Different SLAs (critical = 99.99%, batch = 99%). (3) Independent scaling (search scales to 50 instances, checkout to 5).