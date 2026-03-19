# Модуль 007 — QA: sync.Pool & Object Reuse

## Блок 1: sync.Pool internals

**Q1.1:** `sync.Pool` per-P private — доступ без lock. Горутина на P0: `Get()` из `local[P0].private`. Между Get и Put горутина мигрирует на P1 (preemption point). `Put()` → `local[P1].private`. Объект перешёл с P0 на P1. Это проблема?

**A1.1:** Не проблема для корректности — sync.Pool потокобезопасен. Объект просто оказался в private другого P. При следующем Get на P0: private пуст → shared → steal от P1. Небольшой overhead (steal дороже private access). Но: горутина "pinned" к P на время Get/Put (runtime.pin). Pin предотвращает миграцию на время критической операции. Между Get и Put — нет pin, миграция возможна. На практике: миграция между Get и Put — нормальное явление, sync.Pool обрабатывает корректно.

**Q1.2:** `sync.Pool` не имеет maxSize. Put 1,000,000 объектов по 1MB — 1TB в пуле? Когда GC соберёт?

**A1.2:** 1TB в пуле — да, до следующего GC cycle. GC trigger: когда heap вырос до `GOGC`% от предыдущего live heap (default GOGC=100 → heap удвоился). 1TB аллоцировано → GC сработает (предыдущий live heap наверняка меньше 1TB). При GC: pool → victim, следующий GC → victim cleared. Два GC cycles — pool пуст, 1TB → GC. На практике: GC triggered раньше (heap growth), pool не накопит 1TB. Но: между GC cycles — memory spike. Для bounded: обернуть sync.Pool в свой Pool с счётчиком и maxSize. Или: не Put oversized objects.

**Q1.3:** `pool.New = nil`. `pool.Get()` когда пул пуст — что вернёт?

**A1.3:** `nil`. `Get()` при пустом пуле и `New == nil` — возвращает `nil` (не panic). Type assertion `pool.Get().(*bytes.Buffer)` → `(*bytes.Buffer)(nil)` → nil pointer. Вызов метода на nil → panic (если метод разыменовывает). Правило: всегда устанавливать `New`. Или: проверять `Get()` на nil.

---

## Блок 2: Переиспользование и Reset

**Q2.1:** `bytes.Buffer` из пула. Get → Reset → Write "hello" → Put. Get → Reset → Write "world". Вопрос: backing array буфера — один и тот же между Get/Put? Reset обнуляет данные или только сбрасывает offset?

**A2.1:** Один backing array. `Reset()` сбрасывает `buf.off = 0` и `buf.buf = buf.buf[:0]` (len=0, cap сохранён). Backing array не перевыделяется, не обнуляется. `Write("hello")` записывает в начало того же array. `Write("world")` после второго Reset — тоже в начало. Данные "hello" всё ещё в array (за пределами len), но недоступны через Buffer API. Для security: `buf.Truncate(0)` + explicit zero: `for i := range buf.Bytes() { buf.Bytes()[i] = 0 }` — но это O(n) и обычно не нужно.

**Q2.2:** `SlicePool[*Connection]` — пул слайсов указателей. Get → append connections → Put (с `s[:0]`). Указатели на connections остаются в backing array за пределами len. GC видит их?

**A2.2:** Да, GC видит. Backing array содержит указатели за пределами len но в пределах cap. GC сканирует весь backing array (по cap, не по len). Connections не будут собраны GC пока слайс в пуле. Memory leak: 1000 connections referenced по backing array, хотя слайс "пуст" (len=0). Решение: обнулить перед Put:
```go
func (p *SlicePool[T]) Put(s []T) {
    clear(s[:cap(s)])  // Go 1.21+: обнулить весь cap
    p.pool.Put(s[:0])
}
```
Или: `for i := range s { s[i] = zero }; s = s[:0]`. Для value types (int, string) — не нужно (GC не сканирует non-pointer types в backing array). Для pointer types — обязательно.

**Q2.3:** Пул `*bytes.Buffer`. Горутина A: Get → buf. Горутина A: паника, defer не вызван, buf не Put. Буфер потерян. Pool создаст новый при следующем Get. Это "утечка"?

**A2.3:** Не утечка в строгом смысле: buf на куче, GC соберёт. Но: упущенная возможность переиспользования. Частые паники → pool деградирует в "always new". Для correctness: `defer pool.Put(buf)` сразу после Get. Defer выполняется при panic. Аналог `defer conn.Release()` из модуля 006.

---

## Блок 3: Oversized objects

**Q3.1:** BufferPool size=4KB. Запрос A: write 4KB → Put (accepted). Запрос B: write 10KB → Put (rejected, cap=10KB > 2×4KB). Запрос C: write 7KB → Put. 7KB < 2×4KB=8KB → accepted. Следующий Get: буфер cap=7KB. Для 100-byte запрос — 7KB waste. Это приемлемо?

**A3.1:** Приемлемо — 7KB vs 4KB = 75% overhead, но абсолютно — 3KB waste. Для тысяч буферов: 3KB × 1000 = 3MB — приемлемо. Threshold 2× — компромисс: принимать умеренный рост (до 2×), отвергать катастрофический (>2×). Для stricter: threshold 1.5× или exact match. Для production: зависит от профиля нагрузки. Если 90% запросов 4KB и 10% — 6-8KB → threshold 2× оптимален. Если bimodal (50% 4KB, 50% 1MB) → два пула разного размера.

**Q3.2:** Два пула: `smallPool` (4KB) и `largePool` (64KB). Запрос: нужен буфер. Размер данных неизвестен заранее. Какой пул использовать? Что если данные 5KB (больше small, меньше large)?

**A3.2:** Стратегия: начать с small. Если не хватает — переключиться на large:
```go
buf := smallPool.Get()
_, err := buf.ReadFrom(reader)
if buf.Len() > 4096 {
    // Перерос small — переключиться на large для следующих
    smallPool.Put(buf)  // reject (oversized)
    buf = largePool.Get()
    // ... re-read или copy
}
```
Или: heuristic по Content-Length (если известен). Или: tiered pool с автоматическим routing по размеру. `net/http` делает подобное: маленькие буферы для headers (4KB), большие для body (32KB).

**Q3.3:** sync.Pool без size guard. Worst case: один запрос аллоцирует 1GB буфер, Put в pool. Pool хранит 1GB до GC. Metrics показывают resident memory 1GB при 0 нагрузки. Как диагностировать?

**A3.3:** `pprof heap`: покажет 1GB аллокацию. `runtime.MemStats.HeapInuse` — 1GB. Но pprof может не показать sync.Pool объекты отдельно (они в куче как обычные объекты). Диагностика: добавить метрики в BufferPool: счётчик Get/Put, max observed cap, rejected count. `pool_max_buffer_cap{pool="io"} 1073741824` — сразу видно 1GB буфер. Alerting: `pool_max_buffer_cap > 10 * expected_size`. Или: Add logging в Put: `if buf.Cap() > threshold { log.Warn("oversized buffer", "cap", buf.Cap()) }`.

---

## Блок 4: GC взаимодействие

**Q4.1:** sync.Pool с 1000 буферами. `GOGC=off` (GC disabled). Буферы никогда не очистятся?

**A4.1:** Верно. GC disabled → pool никогда не очищается. Буферы живут вечно. Memory растёт при Put, не уменьшается. Для тестирования: `GOGC=off` показывает peak memory usage без GC interference. Для production: `GOGC=off` не рекомендуется (memory grows unbounded). Для latency-critical: `GOGC=1000` (GC реже) или `debug.SetMemoryLimit()` (Go 1.19+, soft memory limit).

**Q4.2:** Victim cache (Go 1.13+). Первый GC: `local → victim`. Get проверяет victim. Объект найден в victim — перемещается в local? Или используется и исчезает?

**A4.2:** Get из victim: объект извлекается из victim, НЕ перемещается в local. Использован и... если Put вызван — попадает в local (текущий pool). Если не Put — GC следующий cycle → объект в local (если Put был) или потерян. Victim — одноразовый: объект доступен одному Get, после этого — либо возвращён через Put (в local), либо GC'd. Victim даёт "второй шанс" объектам пережить GC, но не вечный.

**Q4.3:** `runtime.GC()` вызван из пользовательского кода. sync.Pool очищается. Горутина в середине обработки: Get (до GC) → processing → Put (после GC). Put кладёт в свежий (пустой) local pool. Это первый объект в pool после GC. Корректно?

**A4.3:** Корректно. GC очищает pool: local → victim, new empty local. Put после GC → объект в новый local. Следующий Get → из local. Pool "перезагрузился" с одним объектом. Постепенно заполнится снова. Transient burst аллокаций сразу после GC (pool пуст → New() для каждого Get), потом стабилизация. Victim cache сглаживает: часть объектов доступна в victim даже после первого GC.

---

## Блок 5: Бенчмарки и профилирование

**Q5.1:** Benchmark показывает 0 allocs/op с pool. Но первая итерация — New() = 1 alloc. Почему итог 0?

**A5.1:** `b.N` — тысячи/миллионы итераций. 1 аллокация на первой итерации, 0 на остальных. `allocs/op = total_allocs / b.N`. `1 / 1,000,000 ≈ 0.000001` — rounds to 0. `testing.B` отображает целые числа для allocs/op. Реально: 0.000001 allocs/op ≈ 0. Для точности: `b.ReportMetric(float64(totalAllocs)/float64(b.N), "allocs/op")` — custom metric с дробной частью.

**Q5.2:** `b.RunParallel` для конкурентного бенчмарка sync.Pool. Код:
```go
b.RunParallel(func(pb *testing.PB) {
    for pb.Next() {
        buf := pool.Get()
        buf.WriteString("test")
        pool.Put(buf)
    }
})
```
Почему `RunParallel` важен для sync.Pool бенчмарка? Что не покажет sequential benchmark?

**A5.2:** Sequential: одна горутина → один P → всегда попадает в `local[P].private` (fast path). 0 contention, максимальная скорость. Нереалистично. Parallel: `GOMAXPROCS` горутин → contention на shared queues, steal от других P. Показывает реальную производительность под нагрузкой. Также: parallel может выявить баги в типизированной обёртке (race на Reset/Write если обёртка некорректна).

**Q5.3:** `runtime.MemStats.TotalAlloc` — monotonic (только растёт). `runtime.MemStats.Alloc` — текущий heap. Для сравнения "с pool vs без pool" — какую метрику использовать?

**A5.3:** `TotalAlloc` — суммарно аллоцировано за период. С pool: TotalAlloc низкий (мало New()). Без pool: TotalAlloc высокий (каждый запрос — аллокация). Показывает нагрузку на аллокатор. `NumGC` — количество GC циклов. Коррелирует с TotalAlloc: больше аллокаций → чаще GC. `Alloc` (текущий heap) — менее показательна: в момент измерения heap может быть чист после GC. Для бенчмарка: `TotalAlloc` и `NumGC` — основные метрики эффективности pool.

---

## Блок 6: Production паттерны

**Q6.1:** HTTP handler: `bufPool.Get()` → marshal JSON → write response → `bufPool.Put()`. Паника в marshal (nil pointer). Defer `bufPool.Put(buf)` установлен. Буфер содержит partial JSON. Recovery перехватит, но буфер возвращён в pool с мусором. Следующий Get: Reset очистит?

**A6.1:** Если Get делает Reset — да, partial JSON очищен. Следующий пользователь получает чистый буфер. Если Reset только в Put (не в Get) — partial JSON остаётся. Это ещё один аргумент за Reset в Get: defensive, даже если Put не сделал cleanup (паника, забытый Put, etc.).

**Q6.2:** `sync.Pool` для `json.Encoder`. `enc := json.NewEncoder(buf)`. Put enc в pool. Get → enc.Encode(data). Проблема: Encoder содержит ссылку на buf. Buf из другого pool. Reset enc — сброс какого состояния?

**A6.2:** `json.Encoder` внутри: `w io.Writer` (buf) + internal state (indentation, error). Put Encoder → Pool хранит ссылку на buf. Buf возвращён в свой pool. Encoder из pool → `w` указывает на буфер другого пользователя или на переиспользованный буфер. Corruption. Правило: не пулить объекты с ссылками на другие пулированные объекты. Pool Encoder: при Get — заменить `w` на свежий buf. Или: не пулить Encoder, пулить только буферы. `encoding/json` внутренне пулит свой `encodeState`, но управляет lifecycle тщательно.

**Q6.3:** Стоит ли добавлять sync.Pool в connection pool (модуль 006) для IO буферов? Acquire → Get buffer → IO → Put buffer → Release. Какой measurable benefit?

**A6.3:** Benefit: каждый Acquire+IO+Release без pool = 1 buffer allocation (4-64KB). С pool: 0 allocations (amortized). Для 10,000 req/sec × 4KB = 40MB/sec allocation rate → significant GC pressure. С pool: ~0 allocation rate для buffers. Measurable: `TotalAlloc` снижается, `NumGC` снижается, p99 latency стабилизируется (меньше GC pauses). Стоит если: IO throughput высокий (>1000 req/sec) И буферы не тривиальные (>1KB). Не стоит если: throughput низкий или буферы маленькие (overhead pool > benefit).