# Интеграция с Traefik: Reality SNI Passthrough и MiddlewareTCP

## 1. Схема проксирования Reality через Traefik

Протокол VLESS/Trojan с расширением **Reality** требует сквозного TCP-проксирования (TCP Passthrough).
Traefik **не должен** терминировать TLS-сертификат, так как Reality маскируется под чужой SNI (например, `dl.google.com`, `yahoo.com`), а аутентификация и обмен ключами происходят непосредственно между клиентом и Xray-core.

### Поток данных:

1. Клиент инициирует TLS ClientHello с SNI маскировки.
2. Traefik перехватывает TCP-пакет на entrypoint (порт 443).
3. Traefik инспектирует SNI без расшифровки TLS.
4. Срабатывает правило `HostSNI(...)` в `IngressRouteTCP`.
5. Трафик пропускается через `MiddlewareTCP` (IP Whitelist / Ban-list).
6. Traefik добавляет заголовок **PROXY Protocol v2** и отправляет TCP-пакет на порт пода Remnanode.
7. Xray-core считывает реальный IP клиента из PROXY protocol и производит валидацию Reality ключей.

---

## 2. Динамический Whitelist и блокировка на стороне Traefik

Traefik предоставляет CRD **`MiddlewareTCP`** для L4-фильтрации на основе IP-адресов.

### Типы фильтрации:

1. **WhiteList (Разрешительный список)**:
   - В `spec.ipAllowList.sourceRange` указываются доверенные подсети/IP клиентов.
   - Любые другие адреса сбрасываются Traefik еще до передачи в Xray.
2. **Dynamic Ban-list (Черный список)**:
   - При обнаружении торрент-активности через Xray Webhook нода передает IP клиента в модуль интеграции `traefik.integration.ts`.
   - Модуль обновляет ресурс `MiddlewareTCP` или динамический файл конфигурации Traefik, исключая нарушителя или помещая его в блок-лист на `blockDuration` секунд.

---

## 3. Архитектура интеграционного модуля `TraefikIntegration`

В кодовой базе Remnanode модуль реализуется через существующую систему интеграций (`src/integration-modules/`):

```typescript
export class TraefikIntegrationService implements INodeIntegration {
  readonly name = 'traefik';

  // Синхронизация манифеста / конфигурации при старте и обновлении
  async sync(options: INodeIntegrationStartOptions): Promise<INodeIntegrationResult>;

  // Временная блокировка IP клиента при детекте торрента
  async banClientIp(ip: string, durationSeconds: number): Promise<void>;

  // Разблокировка по истечении тайм-аута
  async unbanClientIp(ip: string): Promise<void>;

  async stop(): Promise<void>;
}
```

### Способы взаимодействия ноды с Traefik:

1. **Kubernetes CRD Patch (Рекомендуемый для K8s)**:
   - Нода через свой `ServiceAccount` имеет RBAC-права на `patch`/`update` ресурса `middlewaretcps.traefik.io`.
   - При бане IP нода патчит манифест `MiddlewareTCP`. Traefik автоматически перезагружает конфигурацию на лету без даунтайма.
