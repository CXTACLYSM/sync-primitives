# Модуль 007 — sync.Pool & Object Reuse

## Контекст

Connection pool из модуля 006 управляет долгоживущими ресурсами: TCP соединениями, DB connections. `sync.Pool` решает другую задачу — переиспользование короткоживущих объектов: буферов, структур, временных результатов. Объект берётся из пула, используется, возвращается. GC может очистить пул в любой момент — объект не гарантированно выживает между сборками мусора.

Зачем? Аллокация на куче — дешёвая в Go (~25ns), но массовая — дорогая: тысячи аллокаций в секунду создают давление на GC, увеличивают GC pause time, фрагментируют память. `sync.Pool` снижает количество аллокаций: вместо `make([]byte, 4096)` на каждый запрос — `pool.Get()`, использовать, `pool.Put()`. Один буфер обслуживает тысячи запросов.

В connection pool `sync.Pool` применяется для буферов чтения/записи: каждый Acquire нуждается в буфере для IO, pool буферов снижает аллокации на горячем пути.

## Что нужно реализовать

Отдельный пакет `bufpool` для типизированных обёрток и интеграция с `connpool`.

**Пакет `bufpool` — типизированная обёртка над sync.Pool:**

```go
type BufferPool struct {
    pool sync.Pool
    size int
}
```

**Конструктор `NewBufferPool(size int) *BufferPool`.**

**Методы:**

- `Get() *bytes.Buffer` — получить буфер из пула. Если пуст — создать новый. Reset перед выдачей.
- `Put(buf *bytes.Buffer)` — вернуть в пул. Если вырос больше `2 × size` — не возвращать.

**Пакет `slicepool` — пул слайсов с generics:**

```go
type SlicePool[T any] struct {
    pool sync.Pool
    cap  int
}
```

**Методы:**

- `Get() []T` — слайс с len=0, cap=pool.cap.
- `Put(s []T)` — вернуть. `s = s[:0]`.

**Интеграция с connpool:** `BufferPool` в `Pool` для IO буферов.

**Бенчмарки:** with pool vs without pool — аллокации, скорость, GC давление.

**Файл `main.go`** — демонстрация: Get/Put цикл, oversized rejection, GC clearing, MemStats сравнение, бенчмарк результаты.

## Требования

1. `sync.Pool.New` — фабрика при пустом пуле. Установить при создании.
2. Type assertion скрыт внутри типизированных обёрток.
3. Oversized rejection: `buf.Cap() > 2*defaultSize` → не Put.
4. Get всегда возвращает чистый буфер: `Reset()` / `s[:0]`.
5. `sync.Pool` потокобезопасен — без внешней синхронизации.
6. GC очищает pool — это hint для runtime, не кэш.
7. Бенчмарки через `testing.B` с `ReportAllocs`.

## Ожидаемый вывод main.go

```
=== BufferPool ===
Get buffer (cap=4096), write 100 bytes, Put
Get buffer (cap=4096), reused! len=0 (Reset worked)

=== Oversized rejection ===
Get, write 1MB, Put → rejected (too large)
Get → new allocation (oversized not returned)

=== GC clears pool ===
Put 100 buffers. runtime.GC(). Get → new allocation

=== Memory comparison (100,000 iterations) ===
Without pool: Alloc=412MB, NumGC=47
With pool:    Alloc=4MB,   NumGC=3
Reduction: 98% fewer allocations

=== Benchmark ===
WithPool-8      5,000,000   234 ns/op   0 B/op  0 allocs/op
WithoutPool-8   2,000,000   567 ns/op  4096 B/op  1 allocs/op
```

## Эксперимент на разрушение

**Эксперимент 1: Data leak.** Get → write "password=secret" → Put без Reset → Get → read previous data. Security vulnerability. Fix: Reset in Get.

**Эксперимент 2: Memory bloat.** Pool cap=4KB. One request writes 10MB → Put → 10MB буфер в пуле навсегда. 1000 Get/Put с 100 bytes — все используют 10MB. Fix: size check in Put.

**Эксперимент 3: Two-phase GC eviction.** Fill pool 10,000. GC #1 → victim cache. GC #2 → cleared. Verify two GC cycles needed.

## Вопросы до написания кода

1. `sync.Pool` vs connection pool (модуль 006) — фундаментальная разница? Почему нельзя sync.Pool для DB connections?
2. Per-P local pool — зачем? Как связано с GMP scheduler?
3. `Put(nil)` — что произойдёт? `Get()` с New=nil?
4. GC очищает pool — баг или фича? Когда очистка желательна?
5. `bytes.Buffer` vs `[]byte` для пула — когда какой?
6. `sync.Pool` в стандартной библиотеке: `fmt`, `encoding/json` — где используется?