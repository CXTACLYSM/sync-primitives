# Модуль 010 — QA: Production Assembly & Stress Test

## Блок 1: Pipeline интеграция

**Q1.1:** Acquire pipeline: rate limit → bulkhead → pool. Rate limit: Allow() consumed 1 token. Bulkhead: Acquire(ctx, "critical") → timeout (partition full). Token потрачен, запрос не обслужен. При 1000 req/sec и 5% bulkhead rejection — 50 tokens/sec потрачены впустую. Rate limiter показывает "50 requests served", но 50 были rejected downstream. Метрики вводят в заблуждение?

**A1.1:** Да — rate limiter tokens не refundable. Allow() — fire-and-forget: token consumed regardless of downstream success. Метрика RateLimited показывает ТОЛЬКО rate limit rejections. BulkheadRejects — отдельная метрика. Total served = AcquireTotal - AcquireErrors. Rate limiter контролирует входной поток, не выходной. При 5% downstream rejection: effective throughput = 95% of rate limit. Если это проблема: (1) Rate limit → Wait (blocking) вместо Allow (non-blocking). Downstream rejection → Release conn → rate token уже потрачен, но conn freed. (2) Выстроить pipeline: bulkhead check (non-blocking TryAcquire) ПЕРЕД rate limit. Cheap check first. Но: это меняет семантику (rate limit не учитывает rejected requests).

**Q1.2:** Release pipeline: buffer → pool → bulkhead. Pool.Release(conn) — conn оказался мёртвым → pool Close(conn) → pool создаёт нового → factory(ctx). Но ctx из оригинального Acquire — может быть expired. Чей context используется для factory в Release path?

**A1.2:** Pool.Release не вызывает factory напрямую. При Release мёртвого conn: pool.active--, check waiters. Если waiter есть — waiter's goroutine вызовет factory с СВОИМ ctx. Если waiters нет — ничего. Factory не вызывается в Release path — только в Acquire path. Мёртвый conn → active--, slot freed → следующий Acquire → может создать новый. Контекст: всегда Acquire caller's ctx. Release caller не предоставляет ctx — Release не делает IO.

**Q1.3:** Acquire: rate limit passed → bulkhead passed → pool.Acquire timeout → need cleanup bulkhead. Код:
```go
conn, err := pm.pool.Acquire(ctx)
if err != nil {
    pm.bulkhead.Release(partition)
    return nil, err
}
```
Между `pool.Acquire` error и `bulkhead.Release` — другая горутина могла попасть в тот же partition slot? Или slot всё ещё held текущей горутиной?

**A1.3:** Slot held текущей горутиной: bulkhead.Acquire прошёл → sem slot acquired → текущая горутина "владеет" slot. Pool error → bulkhead.Release → slot freed → теперь доступен другим. Между pool error и bulkhead Release — наносекунды (sequential code в одной горутине). Другая горутина не может "украсть" slot потому что slot held. Она ждёт на bulkhead sem.Acquire. После Release — первый waiter получит slot. Всё корректно, нет race.

---

## Блок 2: ManagedConn и lifecycle

**Q2.1:** `ManagedConn.Release()` — idempotent через `atomic.Bool`. Горутина A: `mc.Release()` (first). Горутина B: `mc.Release()` (second, concurrent). A: `released.CompareAndSwap(false, true)` → true. B: `released.CompareAndSwap(false, true)` → false (already true). B returns immediately. Но: A между CAS и actual release (buffer put, pool release, bulkhead release). B's CAS fails → return. A finishes release. Correct?

**A2.1:** Correct. CAS гарантирует: только один выполнит release body. B видит CAS fail → return (no-op). A completes full release. Нет double-release на buffer/pool/bulkhead. Но: sharing ManagedConn между горутинами — design error. ManagedConn предназначен для одного владельца. Atomic CAS — safety net, не intended usage pattern. Если sharing detected (через race detector) — это баг в вызывающем коде.

**Q2.2:** `ManagedConn` содержит `*bytes.Buffer` из BufferPool. Горутина: `mc.Buffer().Write(data)` → process → `mc.Release()`. Release: bufPool.Put(buf). Между Write и Release — panic. Defer mc.Release() вызовется. Buffer содержит partial data. Put buffer в pool. Следующий Get: buffer с чужими partial данными. Security?

**A2.2:** BufferPool.Get() делает buf.Reset() — partial data очищена. Следующий пользователь получает чистый buffer. Если Reset в Put вместо Get — partial data видна следующему (если panic до Put → buf не returned → New на следующем Get → чист). Правильно: Reset в Get (defensive). Обе стратегии (Get Reset и Put Reset) должны быть: Put Reset для нормального flow, Get Reset как fallback для abnormal (panic, forgot Put). Belt and suspenders.

**Q2.3:** `ManagedConn.startTime` — записан при Acquire. Release записывает duration. Между Acquire и Release — горутина yield, GC pause 50ms, другие горутины работают. Recorded duration включает GC pause и scheduling delay. Это "правильная" метрика?

**A2.3:** Это wall-clock duration — включает всё: полезную работу, GC pauses, scheduling delays, IO waits. Это ПРАВИЛЬНАЯ метрика для SLA: пользователь видит wall-clock latency. Для профилирования CPU work — другая метрика (pprof CPU profile). Wall-clock duration = what the user experiences. CPU time = what the server spends. Оба нужны. PoolManager записывает wall-clock (duration), pprof — CPU time.

---

## Блок 3: Stress testing

**Q3.1:** Stress test: 200 горутин × 30 секунд. `go test -race`. Race detector overhead: ~5x. Test duration: ~150 секунд (2.5 минуты). CI pipeline timeout: 5 минут. Достаточно? Что если stress test flaky (иногда passes, иногда fails)?

**A3.1:** 2.5 минуты — допустимо для CI. Buffer: CI timeout 10 минут для safety. Flaky stress test: (1) Timeout-based failures: stress test uses context timeout — если system slower under race detector → more timeouts → higher error rate → assertion fails. Fix: relax error rate threshold under -race (10% → 20%). (2) Goroutine leak detection: NumGoroutine after test — GC goroutines may vary. Fix: larger tolerance (±10 instead of ±5). (3) Non-deterministic scheduling: different goroutine interleavings → different results. Not flaky — different valid executions. If assertions fail only sometimes → race condition exists, just rare. Run 100 times: `go test -race -count=100`.

**Q3.2:** `runtime.NumGoroutine()` before=10, after=10. No leak? Что если 5 горутин leaked и 5 от previous test completed between measurements?

**A3.2:** False negative: leak masked by completion. Mitigation: (1) Measure IMMEDIATELY after test, before any cleanup goroutines complete. (2) Wait 1 second after test, measure again — leaked goroutines still alive, completed ones gone. (3) Use `goleak.VerifyNone(t)` — compares goroutine stacks, not counts. Identifies specific leaked goroutines by their stack trace. More robust than NumGoroutine counting. (4) Multiple measurements: measure at 0s, 1s, 5s after test. Leak → count stable. No leak → count decreases to baseline.

**Q3.3:** Stress test прошёл. 0 races, 0 leaks, 0 deadlocks. Deploy в production. Через 3 дня: deadlock. Почему stress test не поймал?

**A3.3:** (1) **Coverage:** stress test 30s, production 3 days. Rare race conditions triggered by specific scheduling that stress test didn't hit. (2) **Environment:** stress test on 8-core dev machine, production on 64-core server. Different scheduling, different contention patterns. (3) **Load profile:** stress test: uniform random. Production: diurnal pattern, bursts, correlated failures. (4) **External dependencies:** stress test: mock factory (instant). Production: real TCP (variable latency, failures, half-open). (5) **State accumulation:** 3 days of operation: slow memory leak, connection state drift, metric overflow. Stress test не имитирует long-running state. Mitigation: soak test (hours), canary deployment, production monitoring with goroutine/deadlock detection.

---

## Блок 4: pprof и профилирование

**Q4.1:** CPU profile 10 секунд. Top function: `sync.(*Mutex).Lock` — 45% CPU. Это contention на pool мьютексе. 200 горутин Acquire/Release. Как уменьшить contention?

**A4.1:** (1) **Сузить критическую секцию:** Lock → minimal work → Unlock. Factory вне lock (уже сделано в модуле 006). Metrics increment вне lock (уже atomic). (2) **Sharding:** N pools вместо одного. Route by hash(partition). Каждый pool — свой мьютекс, contention / N. (3) **RWMutex:** если Acquire mostly reads idle (check idle, peek) — RLock. Write (modify idle, active) — Lock. Но: Acquire модифицирует state (pop idle, active++), RWMutex не помогает. (4) **Lock-free idle:** atomic pointer на top of idle stack. Pop: CAS. Contention → retry. Сложно, error-prone, но possible для LIFO stack. (5) **Reduce Lock duration:** pre-allocate outside lock, copy data outside lock, do expensive work outside lock. Profile each line inside lock: `list Pool.Acquire` в pprof.

**Q4.2:** Heap profile: `bufpool.New` — 80% allocations. sync.Pool не помогает? Почему?

**A4.2:** Возможные причины: (1) GC слишком частый → pool cleared каждый cycle → New на каждый Get. Check: `runtime.MemStats.NumGC` — если GC every 10ms, pool useless. Fix: `GOGC=200` (реже GC), или `debug.SetMemoryLimit()`. (2) Oversized rejection: все buffers rejected в Put (grew too large). Check: metrics rejected_count. Fix: increase size threshold или fix workload (why buffers grow). (3) Pool per-P miss: горутины мигрируют между P, local pool empty → steal → New. Unlikely with 200 goroutines and 8 P (enough reuse). (4) Profile wrong: New called during warmup, not steady state. Run profile AFTER warmup phase.

**Q4.3:** Goroutine dump: `debug/pprof/goroutine?debug=2`. Вывод:
```
goroutine 47 [semacquire, 5 minutes]:
sync.runtime_SemacquireMutex(...)
sync.(*Mutex).Lock(...)
connpool.(*Pool).Acquire(...)
manager.(*PoolManager).Acquire(...)
main.worker(...)
```
Что это означает? Какой компонент — bottleneck? Как исправить?

**A4.3:** Горутина 47 заблокирована на `sync.Mutex.Lock` в `Pool.Acquire` уже 5 минут. `semacquire` = ожидание runtime semaphore (slow path mutex). Bottleneck: pool мьютекс. 5 минут — не normal contention, это deadlock или extreme starvation. Check: другие горутины тоже на Pool.Acquire Lock? Если да — кто держит Lock? Найти горутину с `Pool.Release` или `Pool.Acquire` в locked state. Deadlock: A holds Lock, waits on something, B waits on Lock. Starvation: Lock holders do expensive work under lock (factory under lock — bug from module 006 experiment). Fix depends on root cause: deadlock → fix lock ordering; starvation → narrow critical section.

---

## Блок 5: DedupDo и метрики

**Q5.1:** DedupDo: leader Acquires conn. 99 followers wait. Leader fn takes 5 seconds. 99 followers blocked 5 seconds. Timeouts: followers ctx timeout 2 seconds. After 2s: 99 followers timeout, return error. Leader completes at 5s, result unused by followers. Wasted work?

**A5.1:** Followers timeout: `singleflight.Do` blocks on WaitGroup. Followers' ctx timeout — but Do doesn't accept ctx! Do блокируется навсегда до wg.Done. Followers ctx expired — но они всё ещё на wg.Wait. Только DoChan + select с ctx.Done позволяет timeout. С Do: followers заблокированы до leader finish (5s). Ctx timeout 2s не сработает внутри Do. Решение: использовать DoChan:
```go
ch := pm.dedup.DoChan(key, fn)
select {
case r := <-ch:
    return r.Val, r.Err
case <-ctx.Done():
    return nil, ctx.Err()
}
```
Теперь followers timeout через 2s. Leader продолжает → result отправлен в ch → никто не читает (buffered 1) → GC.

**Q5.2:** Метрики: `AcquireTotal=1000, AcquireErrors=50, DedupHits=200`. Сколько реальных pool.Acquire вызовов?

**A5.2:** DedupHits=200 → 200 followers не вызвали Acquire (ждали leader). DedupMisses = 1000 - 200 = 800? Нет: DedupDo — отдельный метод от Acquire. AcquireTotal=1000 — прямые Acquire вызовы (не через DedupDo). DedupDo внутри вызывает Acquire для leader. DedupHits = 200 followers не Acquire. Реальных pool.Acquire: 1000 (direct) + (DedupMisses) leaders. Если DedupDo вызван 300 раз (200 shared + 100 leaders): pool.Acquire = 1000 + 100 = 1100. Зависит от accounting: считать DedupDo leaders в AcquireTotal или отдельно. Правильно: DedupDo.leader Acquire increment AcquireTotal. Тогда AcquireTotal включает всех.

**Q5.3:** Stats() вызван из HTTP handler (`/debug/pool/stats`). Stats читает atomic метрики и pool.Stats() (под мьютексом). Конкурентный доступ: 200 горутин Acquire/Release + Stats read. Stats blocked на pool mutex если Acquire holds it?

**A5.3:** Pool.Stats(): `p.mu.Lock()` — exclusive. Конкурирует с Acquire/Release. Если pool busy — Stats blocked. Duration: Stats reads `active`, `len(idle)`, copies waiters info — fast (microseconds). Не bottleneck. Но: если Stats called frequently (every 100ms from monitoring) — adds contention. Fix: Pool.Stats() с RLock (read-only operation, multiple readers OK). Или: Stats из cached snapshot (update periodically, read without lock). Для PoolMetrics (atomic): Stats reads atomics → no lock, no contention. Fast path.

---

## Блок 6: Production readiness

**Q6.1:** PoolManager deployed. Мониторинг dashboard. Какие графики создать? Какие алерты?

**A6.1:** Графики: (1) `pool_active / pool_max` — utilization gauge. (2) `acquire_rate` — req/sec (derive from AcquireTotal). (3) `error_rate` — errors/total. (4) `acquire_duration_p99` — latency. (5) Per-partition active/max. (6) `rate_limited_total` rate. (7) `dedup_hit_ratio` — dedup efficiency. Алерты: (1) `pool_active/pool_max > 0.9` for 5min → near exhaustion. (2) `error_rate > 0.05` for 1min → too many failures. (3) `acquire_duration_p99 > 1s` → severe latency. (4) `rate_limited_total` rate > 10/sec → rate limit too aggressive or traffic spike. (5) `goroutine_count` increasing → leak.

**Q6.2:** PoolManager в Docker container. Memory limit 512MB. Pool maxConns=100. Каждый conn ~ 1MB (TCP buffers, TLS state). BufferPool buffers 4KB × 100 = 400KB. Горутины: 200 × 8KB = 1.6MB. Total: ~102MB for pool resources. Remaining: 410MB for heap, stack, runtime. Достаточно?

**A6.2:** 102MB for pool — 20% of 512MB. Remaining 410MB — for application heap (data processing), GC overhead (GC needs ~2x live heap for efficient collection), runtime structures. If application heap steady ~100MB → GC needs ~200MB → 300MB used → 210MB free. Tight but workable. Concern: GC pressure with 100 conns × 1MB + application heap. GC frequency increases as heap approaches limit → GC CPU overhead. `debug.SetMemoryLimit(450<<20)` — soft limit, Go reduces heap before hitting container OOM. Monitor: `container_memory_usage_bytes / container_memory_limit_bytes > 0.85` → alert.

**Q6.3:** PoolManager + graceful shutdown. Kubernetes sends SIGTERM. 30 seconds grace period. PoolManager.Close() called. Active connections: 20. Each processing request takes 1-10 seconds. After Close: rate limiter closed (new requests rejected), bulkhead closed (waiting rejected), pool closed (idle closed). Active 20 conns: owners call Release → conn closed (pool closed). If owner takes 15 seconds: Release at 15s, within 30s grace. If owner takes 45 seconds: Kubernetes SIGKILL at 30s. Connection not released, TCP RST. Database sees abrupt disconnect. How to handle?

**A6.3:** (1) Pool.Close with drain timeout: wait up to 25s for active→0. After 25s: force close active connections (interrupt IO). 5s buffer before SIGKILL. (2) Request context from HTTP server: `server.Shutdown(ctx)` sets request contexts done → handlers check ctx.Done → early return → Release. (3) Pre-stop hook: Kubernetes `preStop` → readiness probe fails → no new traffic → drain existing → SIGTERM → Close. (4) Connection-level timeout: each conn has max usage time. If exceeded → pool force-closes. (5) Accept data loss: for non-transactional operations. Log warning, metric increment. For transactional: database rollback on disconnect (server-side timeout).