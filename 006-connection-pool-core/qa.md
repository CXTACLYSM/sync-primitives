# Модуль 006 — QA: Connection Pool Core

## Блок 1: Acquire алгоритм

**Q1.1:** Acquire: idle пуст, active=9, maxSize=10. Горутина A начинает factory (active=10, вне мьютекса). Горутина B вызывает Acquire: idle пуст, active=10 (== maxSize) → встаёт в очередь. Factory A успешно. A возвращает conn. B ждёт. Никто не вызвал Release. B зависла. Сколько активных? Как B получит conn?

**A1.1:** Active=10 (9 изначальных + 1 от A). B ждёт в очереди. B получит conn только когда кто-то вызовет Release (или conn A будет Release). Если A использует conn и Release → Release проверяет waiters → B первый → conn передаётся B. Если A забыла Release → B зависает навсегда (или timeout через ctx). Это подчёркивает критичность `defer conn.Release()` — без defer один забытый Release блокирует всю очередь.

**Q1.2:** Idle содержит 3 conn. Первые два — мёртвые (IsAlive=false). Третий — живой. Acquire по LIFO: берёт последний (третий, живой). Или берёт последний pushed (зависит от порядка push)? Как LIFO взаимодействует с проверкой IsAlive?

**A1.2:** LIFO: последний в слайсе = последний возвращённый = самый свежий. Acquire: pop `idle[len-1]`, проверить IsAlive. Если мёртв → Close, попробовать следующий (`idle[len-2]`). Цикл пока не найдёт живой или idle не опустеет. В описанном сценарии: pop #3 (последний pushed), IsAlive=true → вернуть. Мёртвые #1, #2 остаются в idle — health check уберёт. Если бы #3 тоже мёртв → pop #2, мёртв → Close, pop #1, мёртв → Close → idle пуст → создать новый (если active < maxSize).

**Q1.3:** maxSize=5. Все 5 active. 100 горутин в очереди waiters. Одна Release → conn передаётся первому waiter. Waiter работает 1ms → Release → второй waiter. Throughput очереди: ~1000/sec (1ms per conn). 100 waiters × 1ms = 100ms. Это sequential — одно соединение обслуживает одного за раз. Как увеличить throughput?

**A1.3:** Bottleneck: все 5 conn active, только Release создаёт throughput. Увеличить throughput: (1) Увеличить maxSize → больше параллельных conn. (2) Уменьшить время использования conn (оптимизировать запросы). (3) Connection multiplexing — одно conn обслуживает несколько запросов (HTTP/2, gRPC). (4) Pre-warm: при старте создать maxSize conn в idle, не ждать demand. Для 100 waiters с 5 conn по 1ms: parallel processing = 5 × 1000/sec = 5000/sec. 100/5000 = 20ms total. Не 100ms — waiters обслуживаются 5 параллельно, не 1.

---

## Блок 2: Factory вне мьютекса

**Q2.1:** Factory вне мьютекса. Два goroutines одновременно решили создать conn (оба видели active < maxSize). active++ для каждого. active = maxSize + 1 > maxSize. Инвариант нарушен?

**A2.1:** Нет — если active++ под мьютексом. Горутина A: Lock, active=9 < 10, active=10, Unlock, factory. Горутина B: Lock, active=10 == maxSize → wait queue. active++ атомарно под мьютексом, две горутины не могут одновременно увеличить. Второй видит результат первого. Если бы active++ был вне мьютекса — race. Порядок: Lock → check → active++ → Unlock → factory. Check + increment = одна критическая секция.

**Q2.2:** Factory(ctx) получает context от Acquire. Context отменён во время factory (TCP connect). Factory возвращает ошибку. `active--` под мьютексом. Waiter пробуждается, тоже вызывает factory с тем же context (уже cancelled). Бесконечный каскад?

**A2.2:** Нет каскада: каждый waiter имеет СВОЙ context. Waiter пробуждён: горутина возвращается из `<-waiter.conn` — но conn не отправлен (factory failed, notification через "создай сам"). Фактически: при factory failure notify waiter не отправляет conn, а сигнализирует "попробуй сам". Waiter пробуждается, проверяет свой ctx → если отменён → return error. Если жив → пытается factory(waiter.ctx). Каждый waiter использует свой context, cancelled contexts быстро отваливаются.

Уточнение: в реализации из condition.md Release/factory failure не пробуждает waiter напрямую. Они уменьшают active → следующий Acquire видит active < maxSize → создаёт. Или: при active-- проверить waiters → если есть, инкремент active обратно, создать conn вне lock, отправить waiter. Factory failure: active-- → notify → waiter делает factory. Waiter с cancelled ctx → factory(cancelled) → immediate error → active-- → notify next. Каскад затухает: cancelled waiters отваливаются быстро.

**Q2.3:** Factory занимает 30 секунд (медленный DNS). maxSize=10. 10 горутин одновременно вызвали Acquire на пустом пуле. Все 10 вызвали factory параллельно. 10 параллельных TCP connects. Это проблема?

**A2.3:** Потенциально: 10 параллельных DNS + TCP к одному серверу — burst нагрузки. Для DB: 10 одновременных подключений — обычно OK. Для rate-limited API: burst может нарушить rate limit провайдера. Решение: ограничить параллельные factory вызовы семафором: `factorySem.AcquireWithContext(ctx)` перед factory. Семафор на 3 → максимум 3 параллельных connect. Остальные ждут. Или: singleflight (модуль 009) — если все 10 подключаются к одному endpoint → одно подключение, 9 ждут результат.

---

## Блок 3: Release и lifecycle

**Q3.1:** Release: conn alive, age=29m, maxLifetime=30m. Возвращается в idle. Через 2 минуты Acquire: conn age=31m > maxLifetime → Close. Потрачено 2 минуты idle на conn который скоро умрёт. Как оптимизировать?

**A3.1:** При Release: проверить remaining lifetime = maxLifetime - age. Если remaining < threshold (например < 1 минуту) → Close сразу, не возвращать в idle. Conn с 1 минутой до смерти — не стоит кэшировать. Threshold: configurable, ~10% maxLifetime. `database/sql` не делает этого (проверяет только при Acquire). Production optimisation: зависит от cost of create vs cost of idle slot.

**Q3.2:** maxIdle=5. 10 горутин Release одновременно. Под мьютексом: каждая проверяет `len(idle) < maxIdle`. Первые 5 → в idle. 6-10 → Close. Порядок определяется мьютексом: кто первый захватит. Горутина #10 с самым свежим conn может Close, а #1 со старым — в idle. LIFO idle "сломан"?

**A3.2:** Нет, LIFO сохраняется для порядка ВНУТРИ idle. Но порядок ПОПАДАНИЯ в idle определяется порядком захвата мьютекса — недетерминирован. Горутина #10 может оказаться в idle, #1 — Close. Или наоборот. Это приемлемо: все 10 conn примерно одного "возраста" (созданы одновременно). Разница в freshness — миллисекунды. Если критично: Release с timestamp sorting (idle всегда отсортирован по lastUsed) — overhead не оправдан для marginal benefit.

**Q3.3:** `Conn.Release()` после `Conn.Close()`. Код:
```go
conn.Close()    // закрыть ресурс
conn.Release()  // вернуть в пул — но ресурс уже закрыт!
```
Что произойдёт?

**A3.3:** Зависит от реализации. Если Release не проверяет closed state: conn с закрытым resource возвращается в idle. Следующий Acquire получает conn, вызывает `resource.Query()` → ошибка (closed resource). Или: IsAlive() → false → Close (повторный) → double-close на resource (может panic). Правильная реализация: Close устанавливает `conn.released = true` (или отдельный `conn.closed = true`). Release проверяет: если closed — не возвращать, только active--. Или: Close вызывает pool.release с флагом "don't return to idle".

---

## Блок 4: Health check и конкурентность

**Q4.1:** Health check горутина: Lock → iterate idle → find stale → Close вне lock. Acquire горутина: Lock → pop idle. Между health check lock и unlock — Acquire не может работать. Health check итерирует 100 idle conns, проверяя каждый: ~100µs per check = 10ms lock held. Acquire blocked 10ms. Приемлемо?

**A4.1:** 10ms lock — ощутимо для latency-sensitive Acquire. Решения: (1) Health check копирует idle в snapshot под lock (fast O(n) copy), unlock, проверяет snapshot вне lock, lock снова, удаляет stale. Lock held: O(n) copy + O(m) delete, не O(n) check. (2) Health check обрабатывает по одному: lock, pop one, unlock, check, lock, decide (return or close), unlock. N итераций × 2 lock/unlock, но каждый lock — O(1). Interleaving с Acquire. (3) Ограничить: health check проверяет max K connections per tick. K=10: 10 checks per tick, не все 100. Spread across ticks.

**Q4.2:** IsAlive() для TCP connection: write 1 byte + read response. Под мьютексом? Вне мьютекса? IsAlive занимает 5ms (network RTT).

**A4.2:** Вне мьютекса. IsAlive — IO, 5ms. Под мьютексом: все Acquire/Release blocked 5ms per check. Паттерн: Lock → pop conn from idle → unlock → IsAlive(conn) → if dead: lock, adjust stats, unlock, close. If alive: lock, push back to idle (or give to waiter), unlock. Conn "вне пула" на время проверки — не idle, не active. Transient state. Если Acquire пришёл в это время — не увидит этот conn (не в idle), создаст новый или подождёт.

**Q4.3:** Health check и Acquire race: health check вытащил conn из idle для проверки (вне lock). Acquire видит пустой idle → создаёт новый. Health check: conn alive → возвращает в idle. Теперь total connections = maxSize + 1?

**A4.3:** Нет — если health check не инкрементит active при извлечении из idle. Idle conn не считается active. Total = active + len(idle). Health check: pop from idle → idle уменьшился, active не изменился. Acquire: idle пуст, active < maxSize → active++, create. Total = (active+1) + (idle-1). Пока health check conn не вернулся — total = active+1 + idle-1. Когда health check возвращает conn: idle+1 → total = active+1 + idle. Exceeded? Нет: active+1 ≤ maxSize (проверено при Acquire). idle может превысить maxIdle — при возврате из health check проверить и Close лишние.

---

## Блок 5: database/sql параллели

**Q5.1:** `sql.DB.SetMaxOpenConns(n)` — аналог нашего maxSize. `sql.DB.SetMaxIdleConns(n)` — maxIdle. `sql.DB.SetConnMaxLifetime(d)` — maxLifetime. `sql.DB.SetConnMaxIdleTime(d)` — maxIdleTime. Какие дефолты в sql.DB? Совпадают ли с нашими?

**A5.1:** `sql.DB` дефолты: MaxOpenConns=0 (unlimited!), MaxIdleConns=2, ConnMaxLifetime=0 (unlimited), ConnMaxIdleTime=0 (unlimited). Наши: maxSize=10, maxIdle=5, maxLifetime=30m, maxIdleTime=5m. sql.DB дефолты опасны в production: unlimited connections → сотни соединений к базе → DB перегружена. Best practice: всегда устанавливать MaxOpenConns (10-50 для OLTP). Наши дефолты — безопаснее.

**Q5.2:** `sql.DB` не имеет явного `Acquire/Release`. `db.Query()` автоматически берёт conn из пула и возвращает при закрытии Rows. Какой паттерн ловит забытый Release?

**A5.2:** `rows.Close()` = Release. `defer rows.Close()` — стандартный паттерн. Забытый Close → conn утекает. `sql.DB` имеет finalizer (runtime.SetFinalizer) — при GC: если Rows не закрыт → warning в лог + закрытие. Но GC недетерминирован — conn может утечь на долго. Лучше: `goleak` (Uber) в тестах — обнаруживает горутину-утечки в том числе от незакрытых ресурсов. Наш пул: explicit Acquire/Release — ответственность на вызывающем. Panic при double Release — защита с одной стороны. Для обнаружения leak (Release не вызван): timeout + metric (WaitDuration растёт → кто-то не возвращает conn).

**Q5.3:** `sql.DB` использует connection pooling для `Prepare`. Prepared statement привязан к конкретному conn. `db.Prepare()` → conn A. Query → conn B (другой!). Statement не на B → DB reprepare. Как это связано с LIFO idle?

**A5.3:** LIFO: последний released → первый acquired. Prepared statement на conn A. A released (idle). B acquired для другого запроса. Новый Prepare → conn C (A idle). Statement на A и C. Чем больше конкурентность, тем больше conn с reprepare overhead. LIFO помогает: при стабильной нагрузке одни и те же conn переиспользуются, statements "тёплые". При burst — новые conn, reprepare. Trade-off inherent в pooling: conn fungibility vs session state.

---

## Блок 6: Edge cases и production

**Q6.1:** Pool с maxSize=1. Горутина A: Acquire, работает 5 секунд. Горутина B: Acquire, timeout 1s → error. Горутина C: Acquire, timeout 10s → ждёт. A Release через 5s → C получает conn. Общее время C: 5s (ждал пока A закончит). Справедливо ли что B таймаутнулась а C получила?

**A6.1:** Справедливо при FIFO: B пришла раньше C. B таймаутнулась через 1s — удалилась из очереди. C ждёт дольше (timeout 10s). A Release через 5s → проверка waiters → B уже нет (timeout) → C первый → conn для C. Если бы B timeout 6s: B ждёт 5s → A Release → B получает (первая в FIFO). Timeout — выбор клиента. Короткий timeout → быстрый fail, не занимает место в очереди для других.

**Q6.2:** Pool closed. 5 active conns. Горутины-владельцы не знают что pool closed — они работают. Через 30 секунд каждая Release. Release проверяет closed → Close conn. Active: 5→4→3→2→1→0. Pool drain complete. Но: между Close и drain — pool.Stats() показывает active > 0. Мониторинг алертит. Как обработать graceful drain?

**A6.2:** Pool.Close() → closed=true → закрыть все idle → drain active: не принудительно, ждать Release. Для мониторинга: pool state = "draining" (closed=true, active>0). Stats включает DrainStarted timestamp. Алерт: "pool draining" вместо "pool error". Timeout: если drain не завершился за N секунд → force close active (Close через resource, не через conn Release). Принудительный close: unsafe (resource может быть mid-operation), но предотвращает вечный drain.

**Q6.3:** Conn получен из пула. Resource — TCP connection. Клиентский код: `conn.Resource().(*net.TCPConn).SetDeadline(time.Now().Add(5*time.Second))`. Deadline остаётся после Release. Следующий Acquire получает conn с чужим deadline. Проблема?

**A6.3:** Да — state leaking. Deadline от предыдущего пользователя влияет на следующего. Через 5s: read/write → deadline exceeded error. Следующий пользователь получает "broken" conn. Решения: (1) Reset state при Release: conn.Resource().(*net.TCPConn).SetDeadline(time.Time{}) — clear deadline. Но: generic pool не знает тип resource. (2) Reset callback: `WithResetFunc(func(Resource) error)` option — вызывается при Release, перед возвратом в idle. Каждый resource type реализует свой reset. (3) Документировать контракт: "не мутировать resource state, используйте wrapper". `database/sql` решает через абстракцию: `*sql.Conn` обёрточка, `sql.DB` управляет session state (reset transaction, cancel statements).