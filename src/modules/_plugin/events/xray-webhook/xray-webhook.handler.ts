import { isIP } from 'node:net';

import { Injectable, Logger } from '@nestjs/common';
import { EventsHandler, IEventHandler } from '@nestjs/cqrs';

import { TypedConfigService } from '@common/config/app-config';
import { CidrMatcher } from '@common/utils/cidr-matcher';
import { formatExecutionTime, getTime } from '@common/utils/get-elapsed-time';
import { TorrentBlockerReportModel, XrayWebhookSchema } from '@libs/contracts/models';

import { NftService } from '../../services/nft.service';
import { PluginStateService } from '../../services/plugin-state.service';
import { UserSuspensionService } from '../../services/user-suspension.service';
import { XrayWebhookEvent } from './xray-webhook.event';

const SOURCE_REGEX = /^(?:(?:tcp|udp):)?(?:\[(.+?)\]|(.+?))(?::(\d+))?$/;
const WEBHOOK_TIMEOUT_MS = 5_000;

@Injectable()
@EventsHandler(XrayWebhookEvent)
export class XrayWebhookHandler implements IEventHandler<XrayWebhookEvent> {
    public readonly logger = new Logger(XrayWebhookHandler.name);
    private readonly trustedProxiesMatcher: CidrMatcher;

    constructor(
        private readonly pluginState: PluginStateService,
        private readonly nftService: NftService,
        private readonly userSuspensionService: UserSuspensionService,
        private readonly configService: TypedConfigService,
    ) {
        const trustedProxiesStr = this.configService.getOrThrow('TRUSTED_PROXIES');
        const cidrs = trustedProxiesStr
            .split(',')
            .map((c) => c.trim())
            .filter(Boolean);
        this.trustedProxiesMatcher = new CidrMatcher(cidrs);
    }

    async handle(event: XrayWebhookEvent) {
        const ct = getTime();
        try {
            if (!this.pluginState.torrentBlocker.isEnabled) return;

            const parsed = await XrayWebhookSchema.safeParseAsync(event.webhook);
            if (!parsed.success) {
                this.logger.error(`Invalid webhook: ${JSON.stringify(parsed.error)}`);
                return;
            }

            this.logger.debug(JSON.stringify(parsed.data, null, 2));

            const webhook = parsed.data;
            const ip = this.extractIp(webhook.source);

            if (!ip || !webhook.email) return;

            const whitelisted =
                this.pluginState.torrentBlocker.isIpIgnored(ip) ||
                this.pluginState.torrentBlocker.isUserIgnored(webhook.email);

            if (whitelisted) {
                return;
            }

            const blockDuration = this.pluginState.torrentBlocker.duration!;
            const isProxiedByTraefik = this.trustedProxiesMatcher.contains(ip);

            let blocked = false;

            if (isProxiedByTraefik) {
                // Client is connected through Reverse Proxy (Traefik with PROXY protocol)
                // Do NOT block this IP with local nftables/sockdestroy (it would drop Traefik itself!)
                // Instead, suspend user directly in Xray Core for the specified duration
                this.logger.warn(
                    `[TORRENT-BLOCKER] Detected torrent from proxied connection (client: ${ip}, user: ${webhook.email}). Suspending user for ${blockDuration}s...`,
                );

                blocked = await this.userSuspensionService.suspendUser(
                    webhook.email,
                    blockDuration,
                );
            } else {
                // Direct connection (e.g. Hysteria 2 / TUIC UDP) -> block in Pod Network Namespace + suspend user
                try {
                    await this.nftService.blockIp(ip, blockDuration);
                    await this.userSuspensionService.suspendUser(webhook.email, blockDuration);
                    blocked = true;

                    this.logger.log(
                        `[TORRENT-BLOCKER] Direct IP: ${ip}, user: ${webhook.email}, blocked: ${blocked}, duration: ${blockDuration}s`,
                    );
                } catch (error) {
                    this.logger.error(`Failed to block direct IP ${ip} in nftables: ${error}`);
                }
            }

            const report: TorrentBlockerReportModel = {
                actionReport: {
                    blocked,
                    ip,
                    blockDuration,
                    willUnblockAt: new Date(Date.now() + blockDuration * 1000),
                    userId: webhook.email,
                    processedAt: new Date(),
                },
                xrayReport: webhook,
            };

            this.pluginState.torrentBlocker.addReport(report);

            const webhookUrl = this.pluginState.torrentBlocker.getWebhookUrl();

            if (webhookUrl) {
                this.sendWebhook(webhookUrl, report);
            }
        } catch (error) {
            this.logger.error(`Error in Event XrayWebhookHandler: ${error}`);
        } finally {
            this.logger.debug(`Webhook handled in: ${formatExecutionTime(ct)}`);
        }
    }

    private extractIp(source: string | null): string | null {
        if (!source) return null;

        const prefixMatch = source.match(SOURCE_REGEX);
        const candidate = prefixMatch ? prefixMatch[1] || prefixMatch[2] : source;

        if (isIP(candidate) === 0) return null;

        return candidate;
    }

    private sendWebhook(url: string, report: TorrentBlockerReportModel): void {
        fetch(url, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify(report),
            signal: AbortSignal.timeout(WEBHOOK_TIMEOUT_MS),
        })
            .then((response) => response.body?.cancel())
            .catch(() => void 0);
    }
}
