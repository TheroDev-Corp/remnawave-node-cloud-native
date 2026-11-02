# Remnawave Node (Cloud-Native Kubernetes HA Edition)

> Высокодоступный (Active-Passive / Hot-Standby) форк официального узла [Remnawave Node](https://github.com/remnawave/node), полностью переписанный на **Go**, оптимизированный для работы в Kubernetes без `hostNetwork`, работающий под непривилегированным пользователем `nobody` (`65534:65534`) на минимальном образе `scratch`.

[![Version](https://img.shields.io/badge/version-v3.5.2--HA-blue.svg)](package.json)
[![Runtime](https://img.shields.io/badge/runtime-Go_1.27_/_scratch-00ADD8.svg)](go-node)
[![Security](https://img.shields.io/badge/security-nobody_(65534)-green.svg)](go-node/Dockerfile)
[![Prometheus](https://img.shields.io/badge/metrics-prometheus_ready-E6522C.svg)](#7-встроенные-метрики-prometheus-metrics_port)
[![Upstream](https://img.shields.io/badge/upstream-remnawave%2Fnode-orange.svg)](https://github.com/remnawave/node)
[![License](https://img.shields.io/badge/license-AGPL--3.0-green.svg)](LICENCE)
[![Kubernetes](https://img.shields.io/badge/kubernetes-ready-326CE5.svg)](charts/remnanode)

---

## 📖 О проекте и переход на Go

Оригинальный [Remnawave Node](https://github.com/remnawave/node) спроектирован для одиночного развертывания на физических серверах или VPS в Docker с параметрами `network_mode: host` и системными привилегиями `cap_add: [NET_ADMIN]`. 

При запуске в современном Kubernetes-кластере возникали критические ограничения:
1. **Конфликты портов и блокировок**: Флаг `network_mode: host` и абстрактный UNIX-сокет (`\0rwnode-lock`) делали невозможным запуск более одной реплики на ноде.
2. **Несовместимость блокировок торрентов с Ingress/Traefik**: Блокировка по IP через `nftables` хоста за обратным прокси приводила к бану самого Ingress-контроллера и отключению всех пользователей.
3. **Холодный старт и потеря сессий**: Панель Remnawave регистрирует пользователей в памяти Xray-core на лету по mTLS. При холодном старте чтение и регистрация тысяч пользователей вызывали простой сервиса.

### Миграция на Go (начиная с релиза v3.5.0-HA)

Для решения архитектурных проблем форк был изначально доработан на TypeScript/NestJS, а затем **полностью переписан на чистом Go** ([`go-node/`](go-node/)) как 100% drop-in замена:
- Все релизные образы, начиная с версии **`v3.5.0-HA`** (включая текущую `v3.5.2-HA`), собираются исключительно из директории [`go-node/`](go-node/).
- Исходный код узла на TypeScript в [`src/`](src/) сохранён в репозитории как замороженная эталонная реализация (reference implementation) для сверки протоколов и кросс-проверок контрактов.
- Стоковая панель управления Remnawave работает с Go-узлом без малейших изменений (пройдены тесты паритета по 54 контрольным сценариям и всем маршрутам API).

---

## 📊 Сравнение: Оригинал vs TS-версия vs Go Cloud-Native HA

| Характеристика | Оригинальный `remnawave/node` | TS-форк (`3.4.5-HA`) | Go Cloud-Native HA (`>= 3.5.0-HA`) |
| :--- | :--- | :--- | :--- |
| **Рантайм и ОС** | Node.js + Debian/Alpine + s6 | Node.js 24 + Debian + s6 | **Чистый Go (статический бинарник) на `scratch`** |
| **Размер образа** | ~450 МБ (110 МБ сжатый) | 469 МБ (115 МБ сжатый) | **117 МБ (всего 31 МБ сжатый)** |
| **Потребление RAM** | ~200–250 МБ (растет с V8 heap) | ~190–260 МБ | **~29–99 МБ (сокращение в 3–4 раза)** |
| **Привилегии** | `root`, `CAP_NET_ADMIN`, `hostNet` | `root` (в контейнере), без `hostNet` | **Непривилегированный `nobody` (`65534:65534`), CapEff 0** |
| **Отказоустойчивость** | Одиночный контейнер (простой) | Active-Passive Hot-Standby | **Active-Passive Hot-Standby + Journal Catch-Up** |
| **Безопасность PeerSync** | Отсутствует | HTTP с открытым `X-Secret-Key` | **Взаимный mTLS + HMAC-SHA256 подпись токенов** |
| **Блокировка торрентов** | `nftables` хоста (ломает Ingress) | Прикладная (только `RemoveUser`) | **Динамическое правило `RW_TB_SUSPENDED_USERS`** (мгновенно режет трафик даже в активных mux/xhttp сессиях) |
| **Восстановление ключей**| Нет | Баги в SS2022 и пользователях из конфига | **Полное восстановление из живого рантайма Xray** |
| **Метрики для мониторинга**| Нет | Нет | **Встроенный Prometheus `/metrics`** (безопасен к `reset: true`) |
| **CVE-поверхность** | Пакеты ОС, npm-зависимости, Node.js | Пакеты ОС, npm-зависимости, Node.js | **Нулевая (scratch, нет шелла, нет пакетного менеджера)** |

---

## 🏛 Архитектура решения

```mermaid
flowchart TD
    subgraph PanelSpace ["Управление"]
        Panel["Панель Remnawave"]
    end

    subgraph K8sCluster ["Кластер Kubernetes"]
        K8sLease["coordination.k8s.io/v1 Lease<br/>(remnanode-leader)"]
        
        subgraph Services ["K8s Services (селектор: role=leader)"]
            SvcApi["remnanode-api (ClusterIP)<br/>Port: 2222 (mTLS Panel API)"]
            SvcInbound["remnanode (ClusterIP)<br/>Ports: 443, Reality/Trojan/WS"]
            SvcDirect["remnanode-direct (LoadBalancer/NodePort)<br/>Ports: UDP Hysteria2 / TUIC"]
        end

        subgraph Headless ["Discovery Service (publishNotReadyAddresses: true)"]
            SvcHeadless["remnanode-headless:3000"]
        end

        subgraph PodActive ["Pod 1 (Active Leader) — nobody:65534"]
            P1Node["Remnanode (Go Binary, scratch)<br/>Role: LEADER"]
            P1Xray["Supervised Xray-Core (Hot in-memory)"]
            P1Health["Plain/TLS Peer Server (:3000)<br/>Ready: 200 OK"]
            P1Prom["Prometheus Metrics (:9464)<br/>/metrics plain HTTP"]
        end

        subgraph PodStandby ["Pod 2 (Hot Standby) — nobody:65534"]
            P2Node["Remnanode (Go Binary, scratch)<br/>Role: STANDBY"]
            P2Xray["Supervised Xray-Core (Hot in-memory)"]
            P2Health["Plain/TLS Peer Server (:3000)<br/>Ready: 200 OK (Xray up)"]
            P2Prom["Prometheus Metrics (:9464)<br/>/metrics plain HTTP"]
        end

        Traefik["Ingress / Traefik Proxy<br/>(Reality SNI Passthrough + ProxyProtocol)"]
        Prometheus["Prometheus / vmagent<br/>(Сбор метрик со всех подов)"]
    end

    subgraph Clients ["Клиенты"]
        ClientTCP["TCP Клиенты (Reality/WS)"]
        ClientUDP["UDP Клиенты (Hysteria2)"]
    end

    %% Управление
    Panel -->|mTLS :2222 (RS256 JWT, zstd)| SvcApi
    SvcApi -->|Трафик только на лидера| P1Node

    %% Мониторинг
    Prometheus -.->|GET /metrics :9464| P1Prom
    Prometheus -.->|GET /metrics :9464| P2Prom

    %% Координация
    P1Node <-->|Heartbeat / Renew| K8sLease
    P2Node -.->|Watch / Acquire on Fail| K8sLease

    %% Синхронизация между репликами
    P1Node -->|PeerSync: HMAC-signed mTLS| SvcHeadless
    SvcHeadless -->|Replicate State & Journal :3000| P2Node
    P2Node -.->|Catch-Up Snapshot & yield :3000| P1Node

    %% Клиентский трафик
    ClientTCP --> Traefik --> SvcInbound -->|role: leader| P1Xray
    ClientUDP --> SvcDirect -->|role: leader| P1Xray
```

---

## 🚀 Ключевые возможности Cloud-Native HA

### 1. K8s Leader Election & Динамическая маркировка подов
- Встроен легковесный клиент координации на базе стандартного API Kubernetes `coordination.k8s.io/v1 Lease` (модуль `internal/ha`). Никаких внешних баз данных или sidecar-контейнеров не требуется.
- **Dynamic Role Labeling**: Под-лидер автоматически назначает себе лейбл `role: leader`, а резервный — `role: standby` через прямые вызовы K8s API.
- Сервисы кластера (`remnanode-api`, `remnanode`, `remnanode-direct`) используют селектор `role: leader`, направляя клиентские соединения и запросы Панели исключительно на активного лидера.

### 2. Архитектура «Горячего резерва» (Hot-Standby) и криптографический `PeerSync`
- Процесс Xray-core **запущен одновременно на всех репликах** (лидере и фолловерах).
- **Криптографическая защита PeerSync**: 
  - В отличие от ранних версий, передававших `SECRET_KEY` открытым текстом в заголовке `X-Secret-Key`, Go-узел связывается с пирами по защищённому взаимному **mTLS** с использованием сертификата узла и CA панели.
  - Каждый запрос подписывается токеном `X-Rw-Peer: v1 ts=... sig=...` (HMAC-SHA256 по ключу, выведенному через HKDF из `SECRET_KEY`, с окном валидности ±30 с и защитой от replay-атак).
- **Catch-up Sync и журнал операций (Journal)**:
  - При старте новый под запрашивает снимок конфигурации у лидера.
  - Go-узел ведёт журнал всех инкрементальных операций (`addUser`, `removeUser`, `suspendUser`), произошедших с момента последнего вызова `xray/start`. При подключении новый под догоняет состояние по журналу, гарантируя, что добавленные на лету пользователи не потеряются при передаче полномочий.
- В случае падения лидера резервный под готов принимать трафик мгновенно: **0 мс задержки на запуск Xray и загрузку ключей**.

### 3. Бесшовный Rollout Restart (Zero-Downtime Handoff)
- При плановом обновлении версии или манифестов (`kubectl rollout restart` / Helm upgrade):
  1. K8s поднимает новый под согласно стратегии `maxSurge: 1, maxUnavailable: 0`.
  2. Новый под выполняет Catch-up синхронизацию с лидером и запускает локальный Xray.
  3. После успешного старта Xray новый под отправляет старому лидеру подписанный запрос на мягкую передачу полномочий (`yieldLeadership`).
  4. Старый лидер освобождает K8s Lease, предотвращает повторный захват на 20 секунд и входит в **10-секундный режим дренажа (`isDrainingState`)**.
  5. В течение этих 10 секунд старый лидер сохраняет лейбл `role: leader` и readiness `200 OK`. Оба пода одновременно присутствуют в эндпоинтах K8s, гарантируя завершение существующих пользовательских сессий и плавное переключение новых соединений.

### 4. Самоисцеление и Crash/OOM Gates
- **`canAcquireLeadership` gate**: Предотвращает захват лидерства подом, чей локальный Xray ещё не готов к обслуживанию (исключает перехват трафика неготовым контейнером).
- **`checkLeaderXrayHealth`**: Если процесс Xray на лидере упал или был убит ядром (OOMKilled), а процесс ноды остался жив:
  - Лидер детектирует отказ локального Xray.
  - Если standby-нода здорова, лидер немедленно слагает полномочия, снимает лейбл и отправляет стендбаю команду на внеочередной промоушен.
- **`demoteStaleLeaderLabel`**: При аварийной смерти пода без корректного завершения (SIGKILL / сбой ноды) новый лидер принудительно очищает устаревший лейбл `role: leader` со старого пода.

### 5. Прикладная блокировка торрентов в мультиплексированных сессиях
- Полностью исключены небезопасные зависимости от `hostNetwork`, `CAP_NET_ADMIN` и `nftables-napi`.
- При фиксации BitTorrent-трафика вебхуком Xray нарушитель удаляется из inbounds (`RemoveUser`), а также в рантайм роутинга Xray через RoutingService динамически инжектируется точечное правило:
  ```json
  {"ruleTag": "RW_TB_SUSPENDED_USERS", "user": ["<suspended_id>"], "outboundTag": "RW_TB_OUTBOUND_BLOCK"}
  ```
- **Мгновенный разрыв внутри сессий**: В отличие от оригинальной логики, где клиенты с открытыми mux/XHTTP сессиями продолжали качать торренты в обход повторной аутентификации, правило маршрутизации отсекает новые запросы внутри уже установленных сессий немедленно и **без перезапуска Xray**.

### 6. Запуск от непривилегированного пользователя (`nobody`)
- Образ собирается на базе чистого `scratch` и выполняется под `USER 65534:65534` (`nobody`).
- Ни одной части узла не требуются root-привилегии:
  - Панельный mTLS API, порты Health, PeerSync и Prometheus слушают без root.
  - Смена активного ядра Xray на кастомное происходит через симлинк `/var/lib/rw-core/current`, доступный на запись пользователю `nobody`.
  - Геобазы и ассеты панели пишутся в `/usr/local/share/xray` (или по пути `XRAY_LOCATION_ASSET`).
  - Логи ведутся в `/var/log/xray`.
- Для прослушивания портов ниже 1024 (например, 443 TCP/UDP) в Pod Security контексте чарта применяется безопасный sysctl `net.ipv4.ip_unprivileged_port_start = 0`.

### 7. Встроенные метрики Prometheus (`METRICS_PORT`)
- Опциональный порт (по умолчанию `9464` в Helm-чарте) поднимает plain HTTP эндпоинт `GET /metrics` со стандартным форматом Prometheus.
- **Безопасность относительно панели**: Панель Remnawave периодически опрашивает трафик со сбросом счётчиков (`reset: true`). Go-узел перехватывает сбросы через единый трекер, сохраняя накопительный итог (monotonically increasing counter), благодаря чему графики в Prometheus не искажаются и не сбрасываются в ноль.
- **Экспортируемые метрики**:
  - `remnanode_info`, `remnanode_ha_leader`, `remnanode_ha_draining` — статус реплики и версия;
  - `remnanode_xray_up`, `remnanode_xray_starts_total`, `remnanode_xray_crashes_total`, `remnanode_xray_uptime_seconds`;
  - `remnanode_xray_memory_alloc_bytes`, `remnanode_xray_memory_sys_bytes`, `remnanode_xray_goroutines`;
  - `remnanode_inbound_users`, `remnanode_xray_online_users` (пользователи с активным подключением);
  - `remnanode_xray_inbound_traffic_bytes_total`, `remnanode_xray_outbound_traffic_bytes_total` (с лейблами inbounds/outbounds);
  - `remnanode_xray_user_traffic_bytes_total` (общая сумма пользовательского трафика без раздувания cardinality);
  - `remnanode_xray_observatory_*` и `remnanode_xray_balancer_*` (задержки, живые кандидаты, выбранные бэкенды балансировщика).

---

## 🔧 Переменные окружения

| Переменная | По умолчанию | Описание |
| :--- | :--- | :--- |
| `SECRET_KEY` | *Обязательно* | Секретный токен подключения, сгенерированный Панелью Remnawave |
| `NODE_PORT` | `2222` | Порт mTLS API для взаимодействия с Панелью Remnawave |
| `HEALTH_PORT` | `3000` | Порт внутреннего HTTP-сервера (пробы K8s и защищённый PeerSync) |
| `METRICS_PORT` | *Не задан* | Порт для Prometheus metrics scraper (`GET /metrics`). В чарте: `9464` |
| `K8S_LEADER_ELECTION_ENABLED` | `false` | Включение режима K8s Leader Election (`true` для HA в кластере) |
| `K8S_LEASE_NAME` | `remnanode-leader` | Имя K8s Lease ресурса для координации реплик |
| `PEER_HEADLESS_SERVICE` | *Не задан* | FQDN Headless-сервиса кластера для поиска пиров (например, `remnanode-headless.remnanode.svc.cluster.local`) |
| `DISABLE_INSTANCE_LOCK` | `false` | Отключение монопольного UNIX-сокета (обязательно `true` в Kubernetes) |
| `TRUSTED_PROXIES` | `10.0.0.0/8,...` | Список доверенных подсетей кластера и прокси для извлечения реального IP |
| `SNI_VERIFICATION` | `false` | Строгая проверка соответствия SNI входящих mTLS соединений |
| `XRAY_LOCATION_ASSET` | `/usr/local/share/xray` | Директория для файлов `geoip.dat`, `geosite.dat` и ассетов панели |
| `ASN_DB` | `/usr/local/share/asn/asn-prefixes.lmdb` | Путь к базе префиксов ASN LMDB (чистый Go-ридер без cgo) |
| `POD_NAME`, `POD_NAMESPACE` | *Не заданы* | Имя и пространство имён текущего пода (пробрасываются через Downward API) |

---

## 📦 Развертывание через Helm

В репозитории подготовлен готовый Helm-чарт в директории [`charts/remnanode`](charts/remnanode).

### 1. Подготовка секретов

Создайте Secret с ключом узла из панели:

```bash
kubectl create namespace remnanode
kubectl create secret generic remnanode-secret \
  --namespace remnanode \
  --from-literal=SECRET_KEY="<ВАШ_SECRET_KEY_ИЗ_ПАНЕЛИ>"
```

### 2. Пример `values.yaml` для HA-установки на Go-узле

```yaml
replicaCount: 2

node:
  image:
    repository: ghcr.io/therodev-corp/remnawave-node-cloud-native
    tag: v3.5.2-HA
    pullPolicy: Always
  apiPort: 2222
  healthPort: 3000
  disableInstanceLock: true
  existingSecret: "remnanode-secret"
  
  service:
    type: ClusterIP
    port: 2222

  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 512Mi

leaderElection:
  enabled: true
  leaseName: remnanode-leader

peerSync:
  enabled: true

# Включение сбора метрик Prometheus
metrics:
  enabled: true
  port: 9464
  podAnnotations: true
  scraperNamespace: monitoring

# Безопасность: работа под nobody без root
podSecurity:
  fsGroup: 65534
  unprivilegedPortStart: 0
  runAsNonRoot: true

# Определение клиентских входящих портов
inbounds:
  - name: reality-in
    port: 443
    protocol: tcp
    mode: ingress-sni # SNI Passthrough через Traefik IngressRouteTCP
    sni:
      - "dl.google.com"
  - name: hysteria2-in
    port: 8443
    protocol: udp
    mode: direct # Прямой трафик UDP через Service к лидеру

# Опциональная автоматическая загрузка геобаз (geoip/geosite/zapret)
geodata:
  enabled: false
```

### 3. Установка или обновление чарта

```bash
helm upgrade --install remnanode ./charts/remnanode \
  --namespace remnanode \
  -f values.yaml
```

---

## 🛠 Баги оригинального форка, исправленные в Go

При разработке `go-node` были выявлены и устранены 9 критических дефектов оригинального TypeScript-кода:

1. **Неверный ключ при восстановлении SS2022**:
   В TS метод `add-users` кэшировал сырой пароль вместо base64, из-за чего после временной блокировки пользователь восстанавливался с невалидным ключом. В Go сохраняется точное представление операции Xray.
2. **Невозможность восстановления пользователей из начального конфига**:
   Пользователи, пришедшие в первом запросе `xray/start`, не имели сохранённого определения в TS и после торрент-бана удалялись навсегда. В Go дефиниции извлекаются прямо из активного Xray-конфига.
3. **Неработающие списки исключений `asList` и CIDR**:
   В TS проверка IP выполнялась по точному строковому равенству в `Set`, из-за чего диапазоны CIDR и префиксы ASN никогда не срабатывали. В Go реализован честный префиксный поиск по CIDR и ASN LMDB базе.
4. **Отсутствие репликации настроек плагинов**:
   Запрос `/node/plugin/sync` в TS не передавался на Standby-ноды и не включался в снапшот. При фейловере новый лидер работал без Torrent-Blocker. В Go настройки плагинов полностью реплицируются.
5. **Утечка `SECRET_KEY` в открытом виде по HTTP**:
   В TS каждый межподовый запрос содержал заголовок `X-Secret-Key` с закрытым ключом ноды и JWT в открытом виде. В Go пиры общаются по mTLS с подписью HMAC-SHA256.
6. **Потеря пользователей, добавленных после старта, при передаче роли**:
   При появлении нового пода TS передавал только базовый снапшот `xray/start`. Пользователи, добавленные динамически через `add-user`, терялись до следующего полного перезапуска. В Go ведётся журнал изменений (Journal Catch-up).
7. **Нарушение порядка репликации**:
   В TS репликация выполнялась через неблокирующий `void fetch()`, что могло приводить к гонке и выполнению `removeUser` раньше `addUser`. В Go синхронизация проходит через строгую последовательную очередь.
8. **Мгновенный разбан при `blockDuration: 0`**:
   В TS значение 0 превращалось в `setTimeout(0)`, мгновенно снимая бан, хотя по контракту плагина 0 означает перманентную блокировку.
9. **Продолжение скачивания торрентов внутри уже открытых сессий**:
   Удаление пользователя из Inbound в Xray не закрывает активные мультиплексированные сессии mux/xhttp. В Go добавлено динамическое инжектирование правила маршрутизации `RW_TB_SUSPENDED_USERS` -> `RW_TB_OUTBOUND_BLOCK`, мгновенно прерывающее трафик нарушителя без перезапуска ядра.

---

## 📁 Структура репозитория

- [`go-node/`](go-node/) — **Основная кодовая база**. Высокопроизводительный Go-узел, тесты паритета, модули HA, Prometheus метрики и Dockerfile на базе `scratch`.
- [`charts/remnanode/`](charts/remnanode/) — Официальный Helm-чарт для развертывания в Kubernetes с поддержкой HA, Ingress, Cilium и метрик.
- [`src/`](src/) — Исходный код узла на TypeScript (NestJS). Сохранён как **замороженная эталонная реализация** (Reference Implementation).
- [`docs/architecture/`](docs/architecture/) — Проектные спецификации и архитектурные решения.

---

## 🧪 Разработка и тестирование

Для запуска тестов Go-узла:

```bash
cd go-node

# Быстрые юнит-тесты и TLS end-to-end
go test ./...

# Тестирование с реальным бинарником xray-core
XRAY_BINARY=/usr/local/bin/xray go test ./internal/node/ ./internal/xray/

# Проверка полного паритета ответов с опубликованным образом TS (54 сценария)
PARITY_TS_IMAGE=ghcr.io/therodev-corp/remnawave-node-cloud-native:3.4.5-HA \
PARITY_GO_IMAGE=remnawave-node-go:latest \
  go test -v -run TestParity ./internal/parity/
```

---

## 👥 Contributors & Upstream

- Исходный проект: [Remnawave Node](https://github.com/remnawave/node)
- Официальная документация Remnawave: [docs.rw](https://docs.rw/)
- Cloud-Native адаптация, архитектура HA и порт на Go: [TheroDev-Corp](https://github.com/TheroDev-Corp)

<p align="center">
  Благодарим контрибьюторов оригинального проекта Remnawave:
</p>
<p align="center">
  <a href="https://github.com/remnawave/node/graphs/contributors">
    <img src="https://contrib.rocks/image?repo=remnawave/node" />
  </a>
</p>
