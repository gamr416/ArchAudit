# ArchAudit — HTTP Stress-тестировщик

Инструмент для нагрузочного тестирования HTTP-сервисов на Go с трёхфазной методологией и автоматическим анализом архитектуры.

## Возможности

- **Трёхфазное тестирование**: EXPLORE → STRESS → RECOVERY
- **Адаптивный пул воркеров**: автоматическая настройка под целевую RPS
- **Высокоточное планирование**: интервал 1мс для точного контроля RPS
- **Поддержка множества URL**: тестирование одного или нескольких endpoints
- **Ограничение RPS**: опциональный лимит запросов в секунду
- **P95 Latency**: отслеживание перцентильной задержки
- **Аналитический модуль**: автоматическое определение узких мест
- **Анализ восстановления**: измерение времени восстановления после пиковой нагрузки
- **Высокая производительность**: пул соединений 10,000+
- **Без внешних зависимостей**: использует только стандартную библиотеку Go

## Установка

```bash
go install github.com/gamr416/archaudit@latest
```

Или соберите вручную:

```bash
git clone https://github.com/gamr416/ArchAudit.git
cd ArchAudit
go build -o archaudit main.go
```

## Использование

### Базовый синтаксис

```bash
archaudit -urls <URL> -duration <ПРОДОЛЖИТЕЛЬНОСТЬ> [опции]
```

### Параметры командной строки

| Параметр | Описание | По умолчанию |
|----------|----------|--------------|
| `-urls` | Список URL через запятую | (обязательно) |
| `-urls-file` | Файл с URL (один на строке) | — |
| `-duration` | Общая продолжительность теста | 2m |
| `-workers` | Количество воркеров (0 = авто) | 0 |
| `-timeout` | Таймаут HTTP-клиента | 10s |
| `-max-rps` | Целевая RPS для стресс-фазы | 1000 |
| `-sla` | Порог SLA для задержки | (не задан) |
| `-explore-step` | Множитель нагрузки для explore | 4 |
| `-resources1` | Ресурсы в фазе explore (серверов/pods) | 1 |
| `-resources2` | Ресурсы в фазе stress | 4 |
| `-stability-err` | Порог ошибок для стабильности | 0.05 (5%) |

## Примеры использования

### Базовый трёхфазный тест

```bash
archaudit -urls http://localhost:8080/api -duration 2m -max-rps 5000
```

### Тест с SLA

```bash
archaudit -urls http://localhost:8080/health -duration 30s -max-rps 1000 -sla 100ms
```

### Тест с масштабированием

```bash
archaudit -urls http://localhost:8080/api -duration 2m -max-rps 5000 -resources1 1 -resources2 8
```

### Высоконагруженное тестирование

```bash
archaudit -urls http://localhost:8080/api -duration 1m -max-rps 20000
```

### Тестирование нескольких endpoints

```bash
archaudit -urls "http://localhost:8080/api/users,http://localhost:8080/api/posts" -duration 2m -max-rps 5000
```

### URL из файла

```bash
archaudit -urls-file urls.txt -duration 5m -max-rps 10000
```

### Файл urls.txt

```
http://localhost:8080/api/endpoint1
http://localhost:8080/api/endpoint2
http://localhost:8080/health
```

## Трёхфазная методология

### 1. Фаза EXPLORE (~25% времени)
- Постепенное увеличение нагрузки
- Определение пределов системы
- Целевая RPS = max-rps / explore-step

### 2. Фаза STRESS (~60% времени)
- Продолжительная пиковая нагрузка
- Поиск точек отказа и узких мест
- Измерение P95 задержки под нагрузкой

### 3. Фаза RECOVERY (~15% времени)
- Сниженная нагрузка (10% от пика)
- Измерение времени восстановления системы

## Метрики архитектурной устойчивости

### Формулы

| Метрика | Формула |
|---------|---------|
| **Scaling Efficiency** | `(RPS₂ / RPS₁) / (Resources₂ / Resources₁)` |
| **Error Rate** | `FailedRequests / TotalRequests` |
| **P95 Latency** | 95-й перцентиль всех задержек |
| **SLA Violations** | `Requests(latency > SLA) / TotalRequests` |
| **Recovery Time** | `T_stable - T_load_end` (при ErrorRate < 5% и P95 < SLA) |

### Пороги

| Метрика | Отлично | Приемлемо | Плохо |
|---------|---------|-----------|-------|
| Scaling Efficiency | >90% | 70-90% | <70% |
| Error Rate | <1% | 1-5% | >5% |
| P95 Latency | <200ms | 200-500ms | >500ms |
| SLA Violations | <1% | 1-3% | >3% |
| Recovery Time | <30s | 30-120s | >120s |

## Интерпретация результатов

### Пример вывода

```
=== ArchAudit ===
URLs: 1 | Duration: 2m | SLA: 200ms | Target RPS: 5000
Resources: 1 -> 4 | Stability Error: 5%

=== EXPLORE Phase ===
Duration: 30s | Target RPS: 1250 | Workers: 1000
--- EXPLORE done: 37500 reqs | RPS: 1250 | P95: 25ms

=== STRESS Phase ===
Duration: 90s | Target RPS: 5000 | Workers: 2500
--- STRESS done: 450000 reqs | RPS: 5000 | P95: 45ms

=== RECOVERY Phase ===
Duration: 30s | Target RPS: 500 | Workers: 500
--- RECOVERY done: 15000 reqs | RPS: 500 | P95: 30ms

=== TEST SUMMARY ===
Total: 502500 | RPS: 4187 | Time: 2m30s
Success: 502350 | Failed: 150 | Data: 125625.0KB
Success Rate: 99.97% | Error Rate: 0.03%
P95 Latency: 45ms | SLA Violations: 0.00% (0)

=== PHASE BREAKDOWN ===
EXPLORE: 37500 reqs | RPS: 1250 | P95: 25ms
STRESS: 450000 reqs | RPS: 5000 | P95: 45ms
RECOVERY: 15000 reqs | RPS: 500 | P95: 30ms

=== ARCHITECTURAL METRICS ===
Scaling Efficiency: 100.0% [excellent]
Error Rate: 0.03% [excellent]
P95 Latency: 45ms [excellent]
SLA Violations: 0.00% [excellent]
Recovery Time: 15s [excellent]

Bottleneck: None detected
```

### Определение узких мест

| Bottleneck | Условие |
|------------|---------|
| Critical Error Rate | Error Rate > 10% |
| High Error Rate | Error Rate > 5% |
| Critical P95 Latency | P95 > 1s |
| High P95 Latency | P95 > 500ms |
| Slow Response | Avg Latency > 500ms |
| SLA Violations | SLA Violations > 5% |
| Low Throughput | RPS < 1000 при >1000 запросов |
| Poor Scaling | RPS_stress < RPS_explore × 1.5 |

## Советы по использованию

### Правильная настройка RPS
- Для API: 1,000-5,000 RPS
- Для микросервисов: 5,000-20,000 RPS
- Для CDN/балансировщиков: 20,000-100,000 RPS

### Настройка SLA
- Критичные API: 50-100ms
- Стандартные API: 100-200ms
- Фоновые задачи: 200-500ms

### Настройка ресурсов
Для корректного расчёта Scaling Efficiency укажите количество ресурсов:
- `-resources1 1` — 1 сервер в фазе explore
- `-resources2 4` — 4 сервера в фазе stress

### Рекомендуемая продолжительность
- Быстрый тест: 30 секунд
- Стандартный тест: 2-5 минут
- Полный анализ: 10+ минут

## Лицензия

MIT
