import fs from 'node:fs';
import https from 'node:https';

import { Injectable, Logger } from '@nestjs/common';

import { TypedConfigService } from '@common/config/app-config';

@Injectable()
export class TraefikService {
    private readonly logger = new Logger(TraefikService.name);
    private readonly enabled: boolean;
    private readonly middlewareName: string;
    private readonly namespace: string;
    private readonly baseUrl: string;
    private readonly token: string;
    private readonly agent: https.Agent;

    // Track active banned IPs and their unban timers
    private readonly bannedIps = new Map<string, NodeJS.Timeout>();

    constructor(private readonly configService: TypedConfigService) {
        this.enabled = this.configService.getOrThrow('TRAEFIK_INTEGRATION_ENABLED');
        this.middlewareName = this.configService.getOrThrow('TRAEFIK_MIDDLEWARE_NAME');

        const host = process.env.KUBERNETES_SERVICE_HOST ?? 'kubernetes.default.svc';
        const port = process.env.KUBERNETES_SERVICE_PORT ?? '443';
        this.baseUrl = `https://${host}:${port}`;

        const tokenPath = '/var/run/secrets/kubernetes.io/serviceaccount/token';
        const caPath = '/var/run/secrets/kubernetes.io/serviceaccount/ca.crt';
        const nsPath = '/var/run/secrets/kubernetes.io/serviceaccount/namespace';

        this.token = fs.existsSync(tokenPath) ? fs.readFileSync(tokenPath, 'utf8').trim() : '';
        const ca = fs.existsSync(caPath) ? fs.readFileSync(caPath) : undefined;
        this.agent = new https.Agent({ ca, rejectUnauthorized: Boolean(ca) });

        this.namespace =
            process.env.POD_NAMESPACE ??
            (fs.existsSync(nsPath) ? fs.readFileSync(nsPath, 'utf8').trim() : 'remnanode');
    }

    public get isEnabled(): boolean {
        return this.enabled;
    }

    /**
     * Add client IP to Traefik block list for `durationSeconds`
     */
    public async banIp(ip: string, durationSeconds: number): Promise<void> {
        if (!this.enabled) return;

        // Clear existing timer if IP was already banned
        const existing = this.bannedIps.get(ip);
        if (existing) {
            clearTimeout(existing);
        }

        this.logger.log(
            `[TRAEFIK] Banning IP ${ip} in MiddlewareTCP "${this.middlewareName}" for ${durationSeconds}s...`,
        );

        await this.patchMiddlewareBan(ip, true);

        if (durationSeconds > 0) {
            const timer = setTimeout(() => {
                void this.unbanIp(ip);
            }, durationSeconds * 1000);
            this.bannedIps.set(ip, timer);
        }
    }

    /**
     * Remove client IP from Traefik block list
     */
    public async unbanIp(ip: string): Promise<void> {
        if (!this.enabled) return;

        this.bannedIps.delete(ip);
        this.logger.log(
            `[TRAEFIK] Unbanning IP ${ip} in MiddlewareTCP "${this.middlewareName}"...`,
        );
        await this.patchMiddlewareBan(ip, false);
    }

    private async patchMiddlewareBan(ip: string, isBan: boolean): Promise<void> {
        try {
            // Traefik MiddlewareTCP CRD: /apis/traefik.io/v1alpha1/namespaces/{ns}/middlewaretcps/{name}
            const path = `/apis/traefik.io/v1alpha1/namespaces/${encodeURIComponent(this.namespace)}/middlewaretcps/${encodeURIComponent(this.middlewareName)}`;

            const getRes = await this.request('GET', path);
            if (getRes.statusCode !== 200) {
                this.logger.warn(
                    `MiddlewareTCP "${this.middlewareName}" not found in namespace "${this.namespace}" (status: ${getRes.statusCode})`,
                );
                return;
            }

            const middleware = JSON.parse(getRes.body);
            if (!middleware.spec) middleware.spec = {};
            if (!middleware.spec.ipAllowList) middleware.spec.ipAllowList = {};
            if (!Array.isArray(middleware.spec.ipAllowList.sourceRange)) {
                middleware.spec.ipAllowList.sourceRange = [];
            }

            const cidr = ip.includes('/') ? ip : `${ip}/32`;

            if (isBan) {
                if (!middleware.spec.ipAllowList.sourceRange.includes(cidr)) {
                    middleware.spec.ipAllowList.sourceRange.push(cidr);
                }
                this.logger.log(`[TRAEFIK] Registered ban for ${cidr}`);
            } else {
                middleware.spec.ipAllowList.sourceRange =
                    middleware.spec.ipAllowList.sourceRange.filter((r: string) => r !== cidr);
                this.logger.log(`[TRAEFIK] Removed ban for ${cidr}`);
            }

            // Put back updated MiddlewareTCP
            await this.request('PUT', path, JSON.stringify(middleware));
        } catch (error) {
            this.logger.error(
                `Failed to patch Traefik MiddlewareTCP: ${error instanceof Error ? error.message : String(error)}`,
            );
        }
    }

    private request(
        method: string,
        path: string,
        body?: string,
    ): Promise<{ statusCode: number; body: string }> {
        return new Promise((resolve, reject) => {
            const url = new URL(path, this.baseUrl);
            const headers: Record<string, string> = {
                Accept: 'application/json',
                Authorization: `Bearer ${this.token}`,
            };

            if (body) {
                headers['Content-Type'] = 'application/json';
                headers['Content-Length'] = Buffer.byteLength(body).toString();
            }

            const req = https.request(
                url,
                { method, headers, agent: this.agent, timeout: 5000 },
                (res) => {
                    let data = '';
                    res.on('data', (chunk) => {
                        data += chunk;
                    });
                    res.on('end', () => resolve({ statusCode: res.statusCode ?? 500, body: data }));
                },
            );

            req.on('error', (err) => reject(err));
            req.on('timeout', () => req.destroy(new Error('K8s API request timeout')));
            if (body) req.write(body);
            req.end();
        });
    }
}
