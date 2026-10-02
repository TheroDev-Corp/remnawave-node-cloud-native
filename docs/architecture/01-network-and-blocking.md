# Архитектура сети и гибридной блокировки торрентов в Kubernetes

## 1. Контекст и проблематика

Исторически Remnawave Node в Docker развертывалась с параметрами:

- `network_mode: host`
- `cap_add: [NET_ADMIN]`

Это позволяло:

1. Избегать двойного NAT и сохранять оригинальный IP клиента.
2. Использовать `nftables-napi` для наполнения IP-сетов `TORRENT_BLOCKER_SET_NAME` на сетевом интерфейсе хоста.
3. Использовать `sockdestroy` для отправки через netlink `inet_diag` команды `SOCK_DESTROY` для немедленного разрыва соединений нарушителя.

### Ограничения в Cloud Native / K8s:

1. **Запрет `hostNetwork: true`**:
   - `hostNetwork` нарушает сетевую изоляцию Kubernetes.
   - Делает невозможным запуск двух реплик на одном хосте из-за конфликта портов (`NODE_PORT`, порты Xray).
   - Приводит к конфликтам абстрактных UNIX-сокетов (`\0rwnode-lock`).
2. **Reverse Proxy (Traefik) и PROXY protocol**:
   - Traefik принимает TCP/Reality/WS/gRPC трафик от клиентов и перенаправляет его в под Xray с включенным `acceptProxyProtocol: true`.
   - Внутри сетевого пространства пода IP-пакеты имеют `src_ip = IP_Traefik` и `dst_ip = IP_Pod`. Реальный IP клиента содержится в полезной нагрузке (заголовок PROXY protocol).
   - Ядро Linux внутри пода **не видит** реальный IP клиента. Блокировка `saddr` в `nftables` пода привела бы либо к нулевому эффекту, либо (при ошибке) к бану самого Traefik и отключению всех клиентов!
3. **Прямой трафик (Hysteria 2 / TUIC UDP)**:
   - Проходит напрямую в под через K8s Service (NodePort или LoadBalancer с `externalTrafficPolicy: Local`).
   - Здесь IP-пакеты имеют `src_ip = Client_IP`.

---

## 2. Модель гибридной блокировки

Сетевой поток делится на два независимых контура:

```
                           ИНТЕРНЕТ (Клиенты)
                                   │
      ┌────────────────────────────┴────────────────────────────┐
      ▼ (TCP: Reality / WS / gRPC)                              ▼ (UDP: Hysteria 2 / TUIC)
┌──────────────────────────────────────┐             ┌─────────────────────────────┐
│       Traefik Reverse Proxy          │             │       K8s Service / LB      │
│  - Entrypoint :443 (TCP HostSNI)     │             │  - externalTrafficPolicy:   │
│  - MiddlewareTCP: Whitelist / Bans   │             │    Local                    │
│  - PROXY Protocol v2 upstream        │             └──────────────┬──────────────┘
└──────────────────┬───────────────────┘                            │
                   │ (TCP: src=Traefik_IP)                          │ (UDP: src=Client_IP)
                   ▼                                                ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ Remnanode Pod (Pod Network Namespace)                                            │
│ Capabilities: CAP_NET_ADMIN (изолирован внутри Pod NetNS)                        │
│                                                                                  │
│ 1. Xray-core детектит BitTorrent -> шлет Webhook на внутренний сокет ноды        │
│ 2. XrayWebhookHandler:                                                           │
│    - Читает реальный IP клиента и userId (email)                                 │
│    - Проверяет trusted proxies (IP Traefik)                                      │
│    - Если источник — прямой UDP (Hysteria):                                      │
│        -> Выполняет локальный nftables drop + sockdestroy внутри Pod NetNS       │
│    - Если источник — проксированный TCP (Traefik):                               │
│        -> Инициирует бан на уровне Traefik MiddlewareTCP / API                   │
│        -> Шлет репорт в Панель для временной блокировки пользователя             │
└──────────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Права `CAP_NET_ADMIN` в Pod Network Namespace

Поду **не выдается** `hostNetwork: true` и `privileged: true`.
Вместо этого выдается только `capabilities.add: ["NET_ADMIN"]`:

```yaml
securityContext:
  capabilities:
    add:
      - NET_ADMIN
    drop:
      - ALL
  readOnlyRootFilesystem: false
  runAsNonRoot: false
```

### Преимущества данного подхода:

- В Linux с версии ядра 3.8+ `CAP_NET_ADMIN` полностью изолирован внутри сетевого пространства имен контейнера (Network Namespace).
- Полномочия распространяются **только** на виртуальный интерфейс `eth0` пода.
- Под не имеет прав изменять правила файрвола ноды Kubernetes или других подов.
- Локальный `nftables-napi` и `sockdestroy` успешно отрабатывают для всех прямых UDP-соединений Hysteria, сбрасывая пакеты нарушителя.

---

## 4. Защита от бана Traefik (Trusted Proxies)

Для предотвращения ошибочного блокирования узлов прокси в конфигурацию ноды добавляется список доверенных подсетей/IP:

```env
TRUSTED_PROXIES="10.244.0.0/16,10.96.0.0/12,172.16.0.0/12"
```

Логика в `XrayWebhookHandler`:

1. Если адрес источника попадает в `TRUSTED_PROXIES`:
   - Локальный `nftService.blockIp(sourceIp)` **не вызывается**.
   - Событие передается в модуль интеграции с Traefik для обновления banlist в `MiddlewareTCP`.
   - Событие отправляется в Панель Remnawave с флагом `proxied: true` для блокировки учетной записи пользователя на уровне Xray/панели.
