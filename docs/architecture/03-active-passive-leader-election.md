# Архитектура Active-Passive (Hot-Standby) и K8s Leader Election

## 1. Концепция Hot-Standby (Горячий резерв)

В традиционной схеме холодного резерва пассивная нода держит процессы выключенными. **Для Remnawave это неприемлемо** по следующим причинам:

1. **Динамический Handler API**: Панель Remnawave добавляет, обновляет и удаляет пользователей в рантайме по mTLS (`POST /handler/add-user`, `POST /handler/add-users`). Пользователи регистрируются непосредственно в оперативной памяти запущенного Xray-core через XTLS API.
2. **Время холодного старта**: При падении или перезапуске активной ноды холодный запуск Xray с чтением тысяч пользователей привел бы к простою (downtime) и рассинхронизации клиентских ключей.
3. **Отсутствие коллизий портов в K8s**: Поскольку `hostNetwork: false`, каждый под имеет собственный сетевой namespace и уникальный Pod IP (`10.244.x.x`). Порты `443/TCP`, `443/UDP`, `2222/TCP` **не конфликтуют** между репликами.

Следовательно, применяется архитектура **Hot-Standby**:

- Xray-core **запущен на обеих репликах одновременно**.
- На обеих репликах в памяти Xray находится **полный актуальный набор пользователей**.
- Клиентский трафик направляется Traefik и K8s Service **только на Active-ноду (Лидера)**.
- При перезапуске активного пода переключение происходит мгновенно (**0 мс задержки на инициализацию Xray**).

---

## 2. Архитектура взаимодействия и репликации состояния

```
                            ПАНЕЛЬ REMNAWAVE (mTLS)
                                       │
                                       ▼ (POST /xray/start, /handler/add-user)
                      ┌─────────────────────────────────┐
                      │  K8s Service (remnanode-api)    │
                      └────────────────┬────────────────┘
                                       │
                                       ▼
                       ┌───────────────────────────────┐
                       │        POD 1 (Leader)         │
                       │ ───────────────────────────── │
                       │ - Status: LEADER              │
                       │ - Xray-core: RUNNING (Hot)    │
                       │ - Ready Probe: 200 OK         │
                       │ - PeerSyncService: Master ────┼──────────┐ (Репликация команд)
                       └───────────────┬───────────────┘          │
                                       │                          ▼
                                       │               ┌───────────────────────────────┐
                                       │               │        POD 2 (Follower)       │
                                       │               │ ───────────────────────────── │
                                       │               │ - Status: STANDBY             │
                                       │               │ - Xray-core: RUNNING (Hot)    │
                                       │               │ - Ready Probe: 503 (Standby)  │
                                       │               │ - PeerSyncService: Slave      │
                                       │               └──────────────┬────────────────┘
                                       │                               │
         ┌─────────────────────────────┴───────────────────────────────┘
         │
         ▼
     [TRAEFIK / K8S SERVICE: remnanode-active-service]
     Клиентский входящий трафик направляется исключительно на Pod 1.
     При ролл-ауте Pod 1 -> Traefik мгновенно переключает соединения на Pod 2.
```

---

## 3. Механизм синхронизации состояния (PeerSync)

Чтобы Xray на Standby-поде всегда содержал актуальных пользователей, внедряется модуль **`PeerSyncService`**:

### 3.1. Репликация мутирующих операций в рантайме

Когда Панель отправляет вызовы на Active-под:

- `POST /xray/start`
- `POST /handler/add-user`, `POST /handler/add-users`
- `POST /handler/remove-user`, `POST /handler/remove-users`
- `POST /plugins/sync`

Active-нода:

1. Выполняет операцию локально (применяет в свой `InternalService` и пушит в локальный Xray через XTLS API).
2. Параллельно/асинхронно дублирует запрос на Follower-под через K8s Headless Service (`http://remnanode-headless:2222`).
3. Follower-под применяет изменения к своему локальному Xray.

### 3.2. Синхронизация при старте нового пода (Catch-up Sync)

Когда запускается новый под (например, при `kubectl rollout restart` или масштабировании):

1. Под определяет, что в кластере уже есть активный Лидер.
2. Вызывает внутренний эндпоинт активного пода: `GET /internal/sync-state`.
3. Получает текущий снимок конфигурации Xray, хэши inbounds и список активных пользователей.
4. Загружает их в свой локальный Xray-core и переходит в статус горячего резерва (`Hot-Standby`).

### 3.3. Требования к K8s Headless Service (`publishNotReadyAddresses`)

Для обнаружения Standby-подов через DNS Headless Service **критически важно** включить `publishNotReadyAddresses: true` в манифесте Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: remnanode-headless
  namespace: remnanode
spec:
  clusterIP: None
  publishNotReadyAddresses: true # КРИТИЧНО: позволяет лидеру видеть IP standby-подов до прохождения ими Ready probe
  selector:
    app: remnanode
  ports:
    - name: health-internal
      port: 3000
      targetPort: 3000
```

Без директивы `publishNotReadyAddresses: true` CoreDNS K8s не включает в DNS NotReady поды (Standby-под намеренно возвращает 503 на `/health/ready` до промоушена в лидера).

---

## 4. Координация лидерства (K8s Lease) и Rollout Restart

### 4.1. Параметры K8s Lease

Координация строится на базе `coordination.k8s.io/v1`:

- Имя ресурса: `remnanode-leader`
- `leaseDurationSeconds`: 8 сек
- `renewInterval`: 2 сек

### 4.2. Бесшовный сценарий `kubectl rollout restart`

1. **Исходное состояние**:
   - `Pod 1`: Active/Leader (Readiness: `200 OK`, трафик идет сюда).
   - `Pod 2`: Standby/Follower (Readiness: `503 Standby`, Xray запущен и синхронизирован).

2. **Создание нового пода (Pod 3)**:
   - K8s создает `Pod 3` по стратегии `RollingUpdate` (`maxSurge: 1, maxUnavailable: 0`).
   - `Pod 3` стартует, запускает Xray, запрашивает снимок состояния у `Pod 1`, синхронизирует пользователей и переходит в режим Follower.

3. **Завершение старого лидера (Pod 1)**:
   - K8s отправляет `SIGTERM` в `Pod 1`.
   - В NestJS хуке `beforeApplicationShutdown`:
     1. Pod 1 мгновенно переводит свой Readiness в `503 Service Unavailable`.
     2. Освобождает K8s Lease (`releaseLease()`).
     3. Не закрывает Xray немедленно, давая до 5 секунд на завершение существующих соединений (Graceful Drain).

4. **Мгновенный промоушен Follower**:
   - Освобожденный Lease немедленно захватывается `Pod 2`.
   - `Pod 2`:
     1. Получает событие `LeaderPromotedEvent`.
     2. Переводит Readiness Probe в **`200 OK`**.
     3. **Xray запускать не требуется — он уже запущен и содержит всех пользователей.**
   - Traefik и K8s Service перенаправляют новые соединения на `Pod 2`.
   - Задержка переключения составляет ровно время обновления EndpointSlice в K8s (~50-100 мс).

---

## 5. Спецификация Health Probes

```typescript
@Controller('health')
export class HealthController {
  constructor(
    private readonly leaderService: LeaderElectionService,
    private readonly xrayProcess: XrayProcessService,
  ) {}

  // Liveness: проверяет, что Node.js жив и Xray не разбился в крашлуп
  @Get('live')
  async live(@Res() res: Response) {
    const xrayStatus = await this.xrayProcess.getStatus();
    if (xrayStatus.up) {
      return res.status(HttpStatus.OK).send({ status: 'live', xray: 'up' });
    }
    return res.status(HttpStatus.SERVICE_UNAVAILABLE).send({ status: 'xray_down' });
  }

  // Readiness: 200 только для Leader (принимает клиентский трафик).
  // Follower возвращает 503 с заголовком X-Remnanode-Role: standby.
  @Get('ready')
  async ready(@Res() res: Response) {
    if (this.leaderService.isLeader) {
      return res.status(HttpStatus.OK).send({ status: 'ready', role: 'leader' });
    }
    return res.status(HttpStatus.SERVICE_UNAVAILABLE).send({
      status: 'standby',
      role: 'follower',
      message: 'Hot standby replica. Client traffic routed to active leader.',
    });
  }
}
```

---

## 6. Сбор статистики (Stats Collection)

Чтобы статистика трафика в Панели Remnawave не колебалась при переключении:

1. Запросы сбора статистики (`GET /stats`) от Панели всегда приходят на Active-ноду (через ClusterIP Service).
2. Active-нода опрашивает свой локальный Xray Core.
3. При смене лидера счетчики Xray на новом лидере плавно продолжают инкрементироваться с учетом сохраненных оффсетов, исключая скачки графиков в веб-интерфейсе.
