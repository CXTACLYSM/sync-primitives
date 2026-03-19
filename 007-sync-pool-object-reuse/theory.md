# Модуль 007 — Theory: sync.Pool, аллокации и давление на GC

## sync.Pool: что это и что это НЕ

### Определение

`sync.Pool` — потокобезопасный кэш переиспользуемых объектов. API минимален:

```go
var pool = sync.Pool{
    New: func() any {
        return make([]byte, 0, 4096)
    },
}

buf := pool.Get().([]byte)  // получить (type assertion)
// ... использовать buf ...
pool.Put(buf[:0])             // вернуть (сбросить len)
```

### Что sync.Pool НЕ является

**Не connection pool.** sync.Pool не гарантирует сохранность объектов — GC может очистить в любой момент. Для DB connections: потеря при GC = закрытый connection без cleanup. Connection pool (модуль 006) управляет lifecycle, sync.Pool — нет.

**Не кэш.** Кэш хранит данные с TTL. sync.Pool хранит объекты до GC. Нет TTL, нет eviction policy, нет гарантий. Положил 1000 объектов, GC сработал — 0 объектов.

**Не bounded.** Нет maxSize. Put 1 миллион объектов — все хранятся до GC. Memory usage может быть значительной между GC cycles.

### Для чего sync.Pool

Переиспользование **short-lived, interchangeable** объектов на горячем пути:
- Буферы (`[]byte`, `bytes.Buffer`) для IO
- Temporary structs для сериализации/десериализации
- Builder objects (`strings.Builder`)
- Encoder/decoder states

Требование: объект должен быть **fungible** — любой экземпляр из пула эквивалентен любому другому после Reset. Нет identity, нет state dependency.

## Внутренняя архитектура

### Per-P local pools

Go scheduler работает с P (processor) — логическими процессорами. `GOMAXPROCS` определяет количество P. Каждая горутина выполняется на одном P.

`sync.Pool` имеет массив local pools, по одному на каждый P:

```
sync.Pool
├── local[P0]: private *any + shared []any
├── local[P1]: private *any + shared []any
├── local[P2]: private *any + shared []any
└── local[P3]: private *any + shared []any
```

**Get алгоритм:**
1. Определить текущий P (pin goroutine to P)
2. Проверить `local[P].private` — если не nil, забрать (fast path, no lock)
3. Проверить `local[P].shared` — попытаться pop (lock на shared)
4. Перебрать `local[другие P].shared` — попытаться steal (lock на чужой shared)
5. Если нигде нет — вызвать `pool.New()`

**Put алгоритм:**
1. Определить текущий P
2. Если `local[P].private == nil` — положить туда (fast path, no lock)
3. Иначе — push в `local[P].shared`

### Почему per-P

Fast path (private) — без lock, без atomic. Горутина на P0 кладёт и берёт из `local[P0].private` без синхронизации (одна горутина на P в данный момент, pinned). Это ~5-10ns — как доступ к локальной переменной.

Shared — lock-free queue (Go 1.13+: poolDequeue, fixed-size ring buffer). Steal от других P — lock-free. Contention минимален: каждый P работает преимущественно со своим private.

### GC и victim cache

До Go 1.13: каждый GC cycle полностью очищал pool. Объект жил максимум один GC cycle (~2ms при нормальной нагрузке, до секунд при большом heap).

Go 1.13+: **two-phase eviction**. При GC:
1. `victim = local` (текущие объекты → victim cache)
2. `local = new empty pools` (создать пустые)
3. Старый victim (от предыдущего GC) — очищается

Get проверяет: local → victim → New(). Объект выживает один GC cycle в victim. Два GC подряд без Get — объект очищен.

Зачем: стабилизация. Без victim: GC → pool пуст → burst аллокаций → pool заполняется → GC → pool пуст. Oscillation. С victim: после GC объекты доступны в victim → burst аллокаций минимален → следующий GC — victim очищается, local → victim. Сглаживание.

## Типизированные обёртки

### Проблема: any

`sync.Pool` pre-generics API: `Get() any`, `Put(any)`. Type assertion на каждый Get:

```go
buf := pool.Get().(*bytes.Buffer)  // может panic если nil или wrong type
```

### Решение: generic wrapper

```go
type Pool[T any] struct {
    pool sync.Pool
}

func NewPool[T any](newFunc func() T) *Pool[T] {
    return &Pool[T]{
        pool: sync.Pool{
            New: func() any { return newFunc() },
        },
    }
}

func (p *Pool[T]) Get() T {
    return p.pool.Get().(T)
}

func (p *Pool[T]) Put(x T) {
    p.pool.Put(x)
}
```

Type assertion скрыт. Вызывающий код работает с конкретным типом. Compile-time safety.

Нюанс: `Pool[*bytes.Buffer]` — Get возвращает `*bytes.Buffer`. Put принимает `*bytes.Buffer`. Но: `pool.Put(nil)` — допустимо (any = nil). `pool.Get().(T)` где T=*bytes.Buffer и значение nil — вернёт nil без panic. Но `pool.Get().(*bytes.Buffer)` для non-pointer T — panic если nil. Wrapper должен обрабатывать nil.

## Oversized object rejection

### Проблема: memory bloat

```go
buf := pool.Get().(*bytes.Buffer)  // cap=4096
buf.Write(hugeData)                 // buf grows to 10MB
pool.Put(buf)                       // 10MB buffer в пуле
```

Следующий Get получит 10MB буфер. Для 100-byte запроса — waste 99.999%. Если пул содержит 100 таких буферов — 1GB waste.

### Решение: size guard

```go
func (p *BufferPool) Put(buf *bytes.Buffer) {
    if buf.Cap() > p.size*2 {
        return  // не возвращать, пусть GC соберёт
    }
    buf.Reset()
    p.pool.Put(buf)
}
```

Threshold: `2 × default size`. Буфер вырос незначительно — OK, переиспользуем. Вырос катастрофически — discard. Конкретный множитель (2x, 4x, 8x) — trade-off: строже → больше аллокаций (чаще discard), мягче → больше memory waste. 2x — консервативный стандарт.

### Альтернатива: shrink

Вместо discard — уменьшить буфер:

```go
if buf.Cap() > p.size*2 {
    buf = bytes.NewBuffer(make([]byte, 0, p.size))  // новый маленький буфер
}
pool.Put(buf)
```

Trade-off: аллокация нового маленького буфера vs discard + GC + New(). Для простоты: discard.

## Reset: очистка перед выдачей

### Security

Буфер может содержать данные предыдущего пользователя: passwords, tokens, PII. Без Reset: следующий Get видит чужие данные. В multi-tenant системе — cross-tenant data leak.

```go
func (p *BufferPool) Get() *bytes.Buffer {
    buf := p.pool.Get().(*bytes.Buffer)
    buf.Reset()  // очистить ДО выдачи
    return buf
}
```

`Reset` в Get, не в Put: даже если Put забыт (объект просто отброшен) — следующий Get из New() тоже чист. Defensively: Reset при выдаче, не при возврате.

### bytes.Buffer.Reset()

`buf.Reset()` сбрасывает len в 0, сохраняет backing array (cap не меняется). Быстро: O(1), не обнуляет память. Для crypto-sensitive данных: `buf.Reset()` не обнуляет байты — `copy(buf.Bytes(), zeros)` перед Reset для paranoid cleanup. Для обычных буферов — Reset достаточен.

### Slice reset

Для `[]T` нет метода Reset. Сброс: `s = s[:0]` — len=0, cap сохранён. Элементы слайса не обнулены (backing array не трогается). Для `[]*T` (слайс указателей): после `s[:0]` старые указатели в backing array (за пределами len) держат ссылки → GC не может собрать объекты. Правильно:

```go
for i := range s {
    s[i] = zero  // обнулить каждый элемент
}
s = s[:0]
```

Или проще: `clear(s)` (Go 1.21+) — обнуляет все элементы в пределах len.

## Бенчмаркинг аллокаций

### testing.B

```go
func BenchmarkWithPool(b *testing.B) {
    pool := NewBufferPool(4096)
    b.ReportAllocs()
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        buf := pool.Get()
        buf.WriteString("hello world")
        pool.Put(buf)
    }
}

func BenchmarkWithoutPool(b *testing.B) {
    b.ReportAllocs()
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        buf := bytes.NewBuffer(make([]byte, 0, 4096))
        buf.WriteString("hello world")
        // buf уходит в GC
    }
}
```

`b.ReportAllocs()` — вывести allocations/op и bytes/op. Ожидаемый результат: pool — 0 allocs/op (переиспользование), без pool — 1 allocs/op (новый буфер каждый раз).

### runtime.MemStats

```go
var before, after runtime.MemStats
runtime.ReadMemStats(&before)
// ... нагрузка ...
runtime.ReadMemStats(&after)

fmt.Printf("Alloc: %d → %d\n", before.Alloc, after.Alloc)
fmt.Printf("TotalAlloc: %d\n", after.TotalAlloc - before.TotalAlloc)
fmt.Printf("NumGC: %d\n", after.NumGC - before.NumGC)
```

`Alloc` — текущий heap. `TotalAlloc` — суммарно аллоцировано (monotonic). `NumGC` — количество GC cycles. Pool снижает TotalAlloc и NumGC.

## sync.Pool в стандартной библиотеке

### fmt

`fmt.Fprintf` внутри использует `sync.Pool` для `pp` (print state). Каждый вызов `Println` берёт pp из пула, форматирует, возвращает. Без пула: миллионы аллокаций pp при интенсивном логировании.

### encoding/json

`json.Encoder` использует `sync.Pool` для внутренних буферов. `json.Marshal` — аллоцирует `encodeState` из пула.

### net/http

`http.Server` использует sync.Pool для буферов чтения запросов и записи ответов. Каждый HTTP request/response — буфер из пула.

Паттерн повторяется: стандартная библиотека использует sync.Pool на горячих путях где аллокации — bottleneck. Прикладной код — реже, но для high-throughput систем — тот же принцип.

## Вопросы для самопроверки

1. `sync.Pool` per-P private — доступ без lock. Что произойдёт если горутина мигрирует с P0 на P1 между Get и Put? Объект вернётся в pool другого P?

2. Pool содержит 100 буферов. `runtime.GC()`. Сколько буферов осталось? Ещё один `runtime.GC()`. Сколько?

3. `pool.Put(buf)` — Put не обнуляет объект. `pool.Get()` — Get не обнуляет. Обнулять при Put или при Get? Почему Get предпочтительнее?

4. `sync.Pool` для `*http.Request` — хорошая идея? Request содержит context, headers, body. Reset до чистого состояния — возможен?

5. Benchmark: pool 0 allocs/op. Но первая итерация — 1 alloc (pool пуст, New вызван). Почему benchmark показывает 0?