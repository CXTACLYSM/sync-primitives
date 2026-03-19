# Модуль 009 — QA: Singleflight & Thundering Herd

## Блок 1: Singleflight механизм

**Q1.1:** `Do("key", fn)` — 100 горутин. Leader (первая) выполняет fn за 50ms. 99 followers ждут WaitGroup. Leader завершает → `wg.Done()` → 99 горутин разблокированы одновременно. Все 99 читают `c.val` и `c.err`. Data race на чтение?

**A1.1:** Нет data race. `wg.Done()` создаёт happens-before relationship: запись `c.val, c.err = fn()` happens-before `wg.Done()`, `wg.Done()` happens-before `wg.Wait()` return, `wg.Wait()` return happens-before чтение `c.val, c.err`. Go memory model гарантирует: все записи до Done видны после Wait. 99 горутин читают одновременно (read-only) — нет data race. Множественное concurrent read — безопасно. Race detector не сработает.

**Q1.2:** `Do` → leader fn паникует. `c.val, c.err = fn()` — паника не присваивает val/err. `wg.Done()` не вызван (паника прервала выполнение). Все followers ждут `wg.Wait()` навечно. Deadlock?

**A1.2:** Да — deadlock без recovery. `fn()` паникует → stack unwinding → `Do` не вызывает `wg.Done()`. 99 followers навечно на `wg.Wait()`. Решение:
```go
func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
    // ...
    // Leader выполняет fn с recovery
    func() {
        defer func() {
            if r := recover(); r != nil {
                c.err = fmt.Errorf("singleflight panic: %v", r)
            }
        }()
        c.val, c.err = fn()
    }()
    c.wg.Done()
    // ...
}
```
Recovery → err установлен → wg.Done → followers получают ошибку, не deadlock. `x/sync/singleflight`: propagates panic ко ВСЕМ waiters через специальный механизм (каждый waiter re-panics).

**Q1.3:** Singleflight call завершён. `delete(g.calls, key)` выполнен. Горутина X вызывает `Do("key", fn)` МЕЖДУ `delete` и `wg.Done()`. X не находит call в map → становится новым leader → запускает fn параллельно. `wg.Done()` от старого call разблокирует старых followers. Два fn одновременно. Это баг?

**A1.3:** Зависит от порядка: `delete` ДО `wg.Done` или ПОСЛЕ. Если delete ПОСЛЕ Done: X видит call в map → Wait → получает результат (already done, Wait returns immediately) → shared=true. Нет нового fn. Если delete ДО Done (как в `x/sync`): X не видит call → new leader → new fn. Старые followers получают старый результат. Не баг — это design choice. `x/sync` выбирает "delete before Done" чтобы следующий call получил свежий результат. Старые followers — committed к старому результату (уже ждут). Window между delete и Done — наносекунды, вероятность X попадёт в окно — минимальна.

---

## Блок 2: Shared results и мутабельность

**Q2.1:** `fn` возвращает `map[string]int`. 50 горутин получают тот же map. Горутина #10: `result["new_key"] = 42`. Горутины #11-50 читают map. Concurrent read + write на одной map → fatal error. Как защититься?

**A2.1:** Три подхода: (1) fn возвращает immutable данные: `string`, `int`, value types — безопасны. Для map: deep copy перед return. (2) Каждый caller копирует: `myMap := maps.Clone(result.(map[string]int))`. Проблема: caller может забыть. (3) Singleflight обёртка копирует: `Do` wrapper возвращает deep copy каждому caller. Overhead: N копий для N callers. Но: N копий × маленький map дешевле чем N DB queries. Правило: fn в singleflight должен возвращать immutable или caller обязан копировать.

**Q2.2:** `fn` возвращает `*User` (pointer). Leader и 99 followers получают один pointer. Leader: `user.LastAccess = time.Now()`. Followers видят обновлённый LastAccess? Или каждый имеет snapshot?

**A2.2:** Все видят обновлённый LastAccess. Один pointer → один объект в памяти. Модификация через любой pointer — видна всем. Data race: Leader write + Followers read одновременно → race detector сработает. Нет snapshot — singleflight не копирует. Решение: fn возвращает value type `User` (не pointer). Или: caller копирует: `myCopy := *user`. Для immutable structs (все поля value types) — value return достаточен. Для struct с pointer fields — deep copy.

**Q2.3:** Singleflight для `json.Marshal(data)`. fn возвращает `[]byte`. Followers получают тот же `[]byte`. Один follower: `result[0] = 'X'` (модифицирует слайс). Остальные видят повреждённый JSON. `[]byte` — reference type. Как быть?

**A2.3:** `[]byte` — slice header: ptr + len + cap. Все followers получают копию header но один backing array. Модификация через любой header — видна всем. Решение: fn возвращает `string` вместо `[]byte` (`string(jsonBytes)`). Strings immutable в Go. Или: singleflight wrapper делает `slices.Clone(result.([]byte))` для каждого follower. Cost: N copies × len(json). Для 1KB JSON × 100 followers = 100KB — дёшево vs 100 DB queries.

---

## Блок 3: DoChan и context

**Q3.1:** `DoChan` возвращает `<-chan Result`. Caller:
```go
select {
case r := <-ch:
    // result
case <-ctx.Done():
    // timeout
}
```
Timeout сработал. Caller ушёл. Но fn продолжает работу (leader горутина). Результат fn → канал ch (buffered 1) → никто не читает. Goroutine leak? Memory leak?

**A3.1:** Goroutine: leader горутина завершится когда fn завершится. Нет leak — горутина не висит на канале (ch buffered 1, send не блокируется). Memory: ch с одним Result в буфере. GC соберёт ch когда все ссылки потеряны (caller ушёл, leader записал и вернулся). Нет leak. Но: fn продолжает выполняться (потребляет ресурсы). Если fn — DB query → conn pool slot занят. Cancel fn через ctx: если fn принимает context → cancel propagates → fn завершается рано. Без context → fn работает до конца. Best practice: fn должен принимать ctx и проверять ctx.Done().

**Q3.2:** 100 горутин `DoChan("key", fn)`. Каждая создаёт горутину для Wait. 100 горутин ожидающих + 1 leader. Для 1000 ключей: 1000 leaders + 100,000 followers горутин. Scalability?

**A3.2:** Горутины дешёвые (~2-8KB stack), но 100,000 горутин — ~200MB-800MB стеков. Для DoChan: каждый follower → горутина → `wg.Wait()` → парковка (не потребляет CPU). Memory: 100K × 2KB = 200MB. Для `Do` (блокирующий): followers не создают дополнительных горутин — Wait прямо в caller горутине. DoChan overhead: +1 горутина per follower. Для high-fan-out (1000 followers per key): DoChan дороже. Оптимизация: Do когда возможно (проще, меньше горутин), DoChan когда нужен select с cancel.

**Q3.3:** `Do("key", fn)` — leader. `DoChan("key", fn)` — follower (другая горутина). Do-leader и DoChan-follower ожидают одного call. Результат shared. Корректно ли смешивание Do и DoChan для одного ключа?

**A3.3:** Зависит от реализации. `x/sync/singleflight`: Do и DoChan разделяют один `call` struct. Do follower: `c.wg.Wait()`. DoChan follower: горутина с `c.wg.Wait()` → send в канал. Оба ждут один wg.Done(). Корректно. В своей реализации: если Do и DoChan используют одну `calls` map — работает. Если разные maps — два fn для одного key. Правило: одна map для обоих.

---

## Блок 4: errgroup

**Q4.1:** `errgroup.WithContext(ctx)` — derived context. Горутина #1 возвращает error. Context cancelled. Горутина #2 в середине DB query (не проверяет ctx). `Wait()` ждёт #2 до завершения query (30 секунд). Можно ли force-cancel?

**A4.1:** Нет force-cancel горутин в Go. `ctx.Done()` — cooperative: горутина должна проверять. Если горутина в `db.QueryContext(ctx, ...)` — DB driver проверяет ctx → cancel query → return. Если горутина в `db.Query(...)` (без ctx) — не видит cancel, работает до конца. Wait ждёт. Правило: все IO операции внутри errgroup Go-функций должны принимать ctx. Без ctx → неотменяемые, Wait может висеть. Workaround: `g.SetLimit(N)` + timeout на g.Wait context.

**Q4.2:** `errgroup.Wait()` возвращает первую ошибку. 3 горутины вернули ошибки. Как получить все три?

**A4.2:** `errgroup` из `x/sync` не поддерживает multiple errors. Возвращает первую. Для всех ошибок: (1) Собирать в `[]error` с мьютексом:
```go
var mu sync.Mutex
var errs []error
g.Go(func() error {
    if err := check(); err != nil {
        mu.Lock()
        errs = append(errs, err)
        mu.Unlock()
        return err  // trigger cancel
    }
    return nil
})
g.Wait()
combined := errors.Join(errs...)
```
(2) Не использовать errgroup cancel (без WithContext), собирать ошибки, объединять. (3) Custom errgroup с `errors.Join` в Wait. На практике: первая ошибка достаточна для алертинга, остальные — для диагностики. Log all, return first.

**Q4.3:** `errgroup.SetLimit(5)`. 100 `g.Go(fn)`. 5 горутин работают, 95 — заблокированы на внутреннем семафоре errgroup. Если 5 рабочих все паникуют — 95 заблокированных навсегда?

**A4.3:** errgroup recovery: `g.Go` wraps fn в горутину с recovery (зависит от версии). Если panic not recovered: горутина завершается → семафор Release → следующая горутина разблокирована. `Wait` получает panic (или error если recovered). В `x/sync/errgroup` (current): no built-in recovery. Panic propagates, горутина завершается, deferred sem release выполняется → следующая горутина запускается. 95 не заблокированы навсегда — семафор release через defer. Но: panic в горутине без recover → `go` recover невозможен из другой горутины → программа crash если panic reaches top of goroutine stack. errgroup горутина: `go func() { ... }()` — panic = crash. Best practice: defer recover внутри fn.

---

## Блок 5: Singleflight + cache

**Q5.1:** Cache + singleflight:
```go
func GetUser(id string) (*User, error) {
    if user, ok := cache.Get(id); ok {
        return user, nil
    }
    v, err, _ := sf.Do(id, func() (any, error) {
        user, err := db.Query(id)
        if err == nil {
            cache.Set(id, user, 1*time.Minute)
        }
        return user, err
    })
    return v.(*User), err
}
```
1000 goroutines call GetUser("42") при cache miss. 1 DB query. Fn sets cache. 999 получают result. Следующий GetUser("42") — cache hit. Всё хорошо. Но: что если fn returns error? Error cached?

**A5.1:** Error НЕ cached: `if err == nil { cache.Set(...) }` — cache.Set только при успехе. Следующий GetUser("42") — cache miss → singleflight → новый fn → retry DB query. Корректно: error = transient (DB temporary down). Caching error → все callers получают cached error даже когда DB recovered. Но: все 999 followers получили error от текущего call — они не retry, они вернули ошибку upstream. Retry — ответственность upstream (HTTP handler retry, client retry).

**Q5.2:** fn занимает 5 секунд (slow DB). 10,000 горутин ждут. Fn завершается с stale data (DB ответила 5s-ago data). 10,000 горутин получают stale data. Без singleflight: каждая горутина получила бы свежие данные (sequential). Freshness vs efficiency trade-off. Как mitigate?

**A5.2:** (1) TTL на singleflight call: `Forget(key)` через goroutine после N секунд. Новые callers → новый fn с fresh data. Старые — получают stale. (2) Background refresh: fn завершился → результат отдан → goroutine запускает re-fetch (async update кэша). Следующий batch получает свежие данные. (3) Accept staleness: 5s stale data приемлемо для многих use cases (user profile, product catalog). (4) Stale-while-revalidate: вернуть stale из cache немедленно, запустить background revalidation. Первый caller после TTL → singleflight для revalidation, остальные — stale cache.

**Q5.3:** Singleflight key design. `GetUser(id)` — key = id. `SearchUsers(query, page, limit)` — key = ? `query + page + limit` — слишком specific (разные pages = разные keys, no dedup). `query` only — разные pages получат одинаковый result (wrong). Как проектировать key?

**A5.3:** Key = canonical representation запроса, определяющего РЕЗУЛЬТАТ. Одинаковый key ⟺ одинаковый результат. `SearchUsers`: key = `fmt.Sprintf("search:%s:p%d:l%d", query, page, limit)` — разные страницы = разные keys (корректно, разные результаты). Dedup: 100 горутин запрашивают `search:shoes:p1:l20` — один запрос. Другие 50 — `search:shoes:p2:l20` — другой запрос. Правило: key должен быть hash/representation всех параметров влияющих на результат. Не больше (over-specific → no dedup), не меньше (under-specific → wrong results shared).

---

## Блок 6: Production сценарии

**Q6.1:** Microservice A вызывает Service B. 100 concurrent requests к B для одного ресурса. Singleflight в A: 1 request к B, 99 ждут. B имеет rate limit 10 req/sec на клиента. Без singleflight: 100 req → 10 pass, 90 rejected (429). С singleflight: 1 req → pass. Singleflight как implicit rate limit reducer?

**A6.1:** Да — singleflight снижает fan-out к downstream. 100 → 1 = 99% reduction. Rate limit не достигнут. Побочный эффект: singleflight предназначен для dedup, но эффективно снижает нагрузку на downstream. Для rate limited APIs: singleflight + cache = отличная стратегия: первый request → singleflight → 1 API call → cache → subsequent requests от cache. Даже без explicit rate limiter.

**Q6.2:** Singleflight + circuit breaker. Circuit open (backend down). Singleflight: fn = circuit.Do(request). Circuit returns error immediately (open state). 100 goroutines → singleflight → 1 circuit call → error → shared error. Без singleflight: 100 circuit calls → 100 errors (fast, circuit already open). Benefit?

**A6.2:** Minimal benefit при open circuit (call is fast: ~1µs). Benefit при half-open: circuit allows 1 probe request. Без singleflight: 100 goroutines → 100 probe attempts → circuit confused (expected 1 probe). С singleflight: 1 probe, 99 wait → probe succeeds → circuit closes → 99 get success result. Singleflight + circuit breaker: cleaner state transitions, fewer spurious probes. Главный benefit singleflight — при closed circuit (normal operation) и high concurrency: 100 → 1 backend call.

**Q6.3:** Distributed singleflight: 5 серверов за load balancer. Каждый имеет local singleflight. Request для key "X" routed to server A: singleflight dedup on A. Same key "X" routed to server B: separate singleflight on B. Two backend queries for same key. How to achieve cross-server dedup?

**A6.3:** (1) Sticky sessions: load balancer routes all requests for key "X" to same server. Consistent hashing by key. Single server = single singleflight = full dedup. (2) Distributed lock: Redis lock per key. First server acquires lock, queries backend, sets cache. Others wait on lock → cache hit. Overhead: Redis RTT per request. (3) Request coalescing at LB level: LB buffers duplicate requests, forwards one. Specialized LBs (Envoy, nginx with lua). (4) Accept partial dedup: local singleflight reduces 100→1 per server. 5 servers = 5 queries instead of 500. Good enough for most cases.