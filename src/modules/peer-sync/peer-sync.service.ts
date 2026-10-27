import dns from 'node:dns/promises';
import fs from 'node:fs';
import os from 'node:os';

import {
    forwardRef,
    Inject,
    Injectable,
    Logger,
    OnApplicationBootstrap,
    OnApplicationShutdown,
} from '@nestjs/common';
import { CommandBus } from '@nestjs/cqrs';

import { TypedConfigService } from '@common/config/app-config';
import { StartXrayCommand } from '@libs/contracts/commands';

import { UserSuspensionService } from '../_plugin/services/user-suspension.service';
import {
    AddUserRequestDto,
    AddUsersRequestDto,
    RemoveUserRequestDto,
    RemoveUsersRequestDto,
} from '../handler/dtos';
import { HandlerService } from '../handler/handler.service';
import { InternalService } from '../internal/internal.service';
import {
    type ILeaderHealthGate,
    LeaderElectionService,
} from '../leader-election/leader-election.service';
import { XrayService } from '../xray-core/xray.service';

export type TPeerReplicationAction =
    | { type: 'startXray'; body: StartXrayCommand.Request }
    | { type: 'addUser'; body: AddUserRequestDto }
    | { type: 'addUsers'; body: AddUsersRequestDto }
    | { type: 'removeUser'; body: RemoveUserRequestDto }
    | { type: 'removeUsers'; body: RemoveUsersRequestDto }
    | {
          type: 'suspendUser';
          body: { userId: string; durationSeconds: number; unbanAt: number };
      }
    | { type: 'restoreUser'; body: { userId: string } };

export interface IPeerSnapshotResponse {
    xrayConfig: Record<string, unknown> | null;
    startXrayRequest: StartXrayCommand.Request | null;
    xrayUp?: boolean;
}

type TForwardRef<T> = T;

@Injectable()
export class PeerSyncService
    implements OnApplicationBootstrap, OnApplicationShutdown, ILeaderHealthGate
{
    private readonly logger = new Logger(PeerSyncService.name);
    private readonly headlessService: string | undefined;
    private readonly namespace: string;
    private readonly nodePort: number;
    private readonly peerPort: number;
    private readonly secretKey: string;

    private catchUpTimer: NodeJS.Timeout | null = null;
    private yieldRetryTimer: NodeJS.Timeout | null = null;
    private isShuttingDown = false;
    private isCatchingUp = false;
    private isRecoveringLocalXray = false;
    private hasRequestedYield = false;

    constructor(
        private readonly configService: TypedConfigService,
        private readonly leaderElectionService: LeaderElectionService,
        private readonly internalService: InternalService,
        @Inject(forwardRef(() => XrayService))
        private readonly xrayService: TForwardRef<XrayService>,
        @Inject(forwardRef(() => HandlerService))
        private readonly handlerService: TForwardRef<HandlerService>,
        @Inject(forwardRef(() => UserSuspensionService))
        private readonly userSuspensionService: TForwardRef<UserSuspensionService>,
        private readonly commandBus: CommandBus,
    ) {
        this.headlessService = this.configService.get('PEER_HEADLESS_SERVICE');
        this.nodePort = this.configService.getOrThrow('NODE_PORT');
        this.peerPort = this.configService.get('HEALTH_PORT') || 3000;
        this.secretKey = this.configService.getOrThrow('SECRET_KEY');

        const nsFile = '/var/run/secrets/kubernetes.io/serviceaccount/namespace';
        this.namespace =
            process.env.POD_NAMESPACE ??
            (fs.existsSync(nsFile) ? fs.readFileSync(nsFile, 'utf8').trim() : 'remnanode');

        if (!this.headlessService) {
            this.logger.log('PEER_HEADLESS_SERVICE is not configured. Standalone mode active.');
        }

        this.leaderElectionService.registerHealthGate(this);
    }

    async onApplicationBootstrap(): Promise<void> {
        if (!this.headlessService) return;

        // If starting up as Follower, attempt immediate catch-up from Leader
        if (!this.leaderElectionService.isLeader) {
            void this.catchUpFromLeader();
        }

        // Start background polling loop for Standby / Follower pods
        this.startFollowerCatchUpLoop();
    }

    onApplicationShutdown(): void {
        this.isShuttingDown = true;
        if (this.catchUpTimer) {
            clearInterval(this.catchUpTimer);
            this.catchUpTimer = null;
        }
        if (this.yieldRetryTimer) {
            clearTimeout(this.yieldRetryTimer);
            this.yieldRetryTimer = null;
        }
    }

    /**
     * ILeaderHealthGate implementation:
     * Determines if this pod is allowed to acquire the K8s Lease right now.
     * Prevents a pod with unready Xray from stealing leadership when a warm peer exists,
     * while still allowing immediate acquisition on genuine Cold Start.
     */
    public async canAcquireLeadership(): Promise<boolean> {
        // 1. If local Xray is already up and running, we can immediately take over leadership
        if (await this.xrayService.isXrayReady()) {
            return true;
        }

        // 2. If Xray is currently starting or catching up from peer, wait until it finishes
        if (this.xrayService.isStarting || this.isCatchingUp) {
            return false;
        }

        // 3. If we have a cached Xray config locally but Xray is not running, start it first
        const cachedRequest = this.internalService.getLastStartXrayRequest();
        if (cachedRequest) {
            void this.ensureLocalXrayStarted(cachedRequest);
            return false;
        }

        // 4. No local config yet: check if any peer has a running Xray or stored snapshot
        if (!this.headlessService) {
            return true;
        }

        const peerSnapshots = await this.fetchPeerSnapshots();
        const peerWithConfig = peerSnapshots.find(
            (p) => p.snapshot.xrayUp || Boolean(p.snapshot.startXrayRequest),
        );

        if (peerWithConfig) {
            // A peer has active config/Xray -> this is NOT a cold start!
            // Catch up and start local Xray before claiming leadership.
            void this.catchUpFromLeader();
            return false;
        }

        // Genuine Cold Start: no peer has Xray running or configured yet.
        // Allow acquiring lease so the panel can connect and push the initial config.
        return true;
    }

    /**
     * ILeaderHealthGate implementation:
     * Periodically invoked by LeaderElectionService while this pod holds the Lease.
     * Detects if local `rw-core` (Xray) died (e.g. killed by OOM) while `node` stayed alive.
     */
    public async checkLeaderXrayHealth(): Promise<{
        healthy: boolean;
        hasHealthyPeer: boolean;
    }> {
        const cachedRequest = this.internalService.getLastStartXrayRequest();

        // If panel has never sent a config yet (Cold Start) or Xray is legitimately restarting, consider healthy
        if (!cachedRequest || this.xrayService.isStarting || this.isRecoveringLocalXray) {
            return { healthy: true, hasHealthyPeer: false };
        }

        const ready = await this.xrayService.isXrayReady();
        if (ready) {
            return { healthy: true, hasHealthyPeer: false };
        }

        this.logger.warn(
            '[PEER-SYNC] 🚨 Local Xray process on Leader is DOWN (likely OOMKilled or crashed)! Checking standby peers...',
        );

        const peerSnapshots = await this.fetchPeerSnapshots();
        const hasHealthyPeer = peerSnapshots.some((p) => p.snapshot.xrayUp === true);

        // Trigger background restart of local Xray so this pod recovers
        void this.ensureLocalXrayStarted(cachedRequest);

        return {
            healthy: false,
            hasHealthyPeer,
        };
    }

    /**
     * ILeaderHealthGate implementation:
     * Actively wakes up standby peers to claim leadership immediately after leader steps down.
     */
    public async notifyStandbyToPromote(): Promise<void> {
        const peerIps = await this.discoverPeerIps();
        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.peerPort}/internal/leader/promote`;
                await fetch(url, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'X-Secret-Key': this.secretKey,
                    },
                    signal: AbortSignal.timeout(3000),
                });
                this.logger.log(
                    `[PEER-SYNC] Notified standby peer ${peerIp} to promote immediately.`,
                );
            } catch (error) {
                this.logger.warn(
                    `[PEER-SYNC] Failed to notify standby peer ${peerIp} to promote: ${error}`,
                );
            }
        }
    }

    private async ensureLocalXrayStarted(request: StartXrayCommand.Request): Promise<void> {
        if (this.isRecoveringLocalXray || this.xrayService.isStarting || this.isShuttingDown) {
            return;
        }
        this.isRecoveringLocalXray = true;
        try {
            this.logger.log('[PEER-SYNC] Starting/recovering local Xray from stored config...');
            await this.xrayService.startXray(request, '127.0.0.1');
            if (await this.xrayService.isXrayReady()) {
                await this.onLocalXrayBecameReady();
            }
        } catch (error) {
            this.logger.error(`[PEER-SYNC] Failed to recover local Xray: ${error}`);
        } finally {
            this.isRecoveringLocalXray = false;
        }
    }

    private async onLocalXrayBecameReady(): Promise<void> {
        if (this.isShuttingDown || this.leaderElectionService.isLeader) {
            return;
        }

        // Trigger election check in case the lease is currently vacant or expired
        await this.leaderElectionService.triggerElectionNow();

        // If still follower, request graceful handoff if this is a newer rollout pod
        if (!this.leaderElectionService.isLeader) {
            void this.requestLeadershipHandoff();
        }
    }

    /**
     * Periodically check if follower needs to catch up or restart a crashed/OOMed Xray process
     */
    private startFollowerCatchUpLoop(): void {
        if (this.catchUpTimer) return;

        this.catchUpTimer = setInterval(() => {
            void this.runFollowerCheckCycle();
        }, 2500);
    }

    private async runFollowerCheckCycle(): Promise<void> {
        if (this.isShuttingDown || this.leaderElectionService.isLeader) {
            return;
        }

        if (this.isCatchingUp || this.xrayService.isStarting || this.isRecoveringLocalXray) {
            return;
        }

        const isReady = await this.xrayService.isXrayReady();
        if (!isReady) {
            const cachedRequest = this.internalService.getLastStartXrayRequest();
            if (cachedRequest) {
                this.logger.warn(
                    '[PEER-SYNC] Standby Xray process is not running. Restarting from cached config...',
                );
                await this.ensureLocalXrayStarted(cachedRequest);
            } else {
                await this.catchUpFromLeader();
            }
        }
    }

    /**
     * Invoked when this pod is promoted to Leader
     */
    public async onPromotedToLeader(): Promise<void> {
        this.logger.log('[PEER-SYNC] Pod promoted to LEADER. Ensuring local Xray state...');
        this.hasRequestedYield = true;
        if (this.yieldRetryTimer) {
            clearTimeout(this.yieldRetryTimer);
            this.yieldRetryTimer = null;
        }

        if (!(await this.xrayService.isXrayReady())) {
            const cachedRequest = this.internalService.getLastStartXrayRequest();
            if (cachedRequest) {
                this.logger.log(
                    '[PEER-SYNC] Starting Xray on newly promoted leader using stored configuration...',
                );
                await this.xrayService.startXray(cachedRequest, '127.0.0.1');
            } else {
                this.logger.warn(
                    '[PEER-SYNC] Promoted to leader without cached Xray config. Attempting peer catch-up or waiting for panel...',
                );
                await this.catchUpFromLeader();
            }
        }
    }

    /**
     * Called by Leader to replicate an operation to peer Follower pods
     */
    public async replicateToPeers(action: TPeerReplicationAction): Promise<void> {
        if (!this.headlessService) return;

        const peerIps = await this.discoverPeerIps();
        if (peerIps.length === 0) {
            this.logger.debug(
                `[PEER-SYNC] No peer replicas discovered to replicate "${action.type}" (ensure publishNotReadyAddresses is true on headless service).`,
            );
            return;
        }

        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.peerPort}/internal/peer-sync/apply`;
                await fetch(url, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'X-Secret-Key': this.secretKey,
                    },
                    body: JSON.stringify(action),
                    signal: AbortSignal.timeout(5000),
                });
                this.logger.log(`[PEER-SYNC] Replicated action "${action.type}" to peer ${peerIp}`);
            } catch (error) {
                this.logger.warn(
                    `[PEER-SYNC] Failed to replicate "${action.type}" to peer ${peerIp}: ${error}`,
                );
            }
        }
    }

    /**
     * Called by Follower when receiving replicated action from Leader
     */
    public async applyReplicatedAction(action: TPeerReplicationAction): Promise<void> {
        this.logger.log(`[PEER-SYNC] Applying replicated action: "${action.type}"`);

        switch (action.type) {
            case 'startXray':
                this.internalService.setLastStartXrayRequest(action.body);
                if (!this.xrayService.isStarting) {
                    await this.xrayService.startXray(action.body, '127.0.0.1');
                    if (await this.xrayService.isXrayReady()) {
                        await this.onLocalXrayBecameReady();
                    }
                } else {
                    this.logger.debug(
                        '[PEER-SYNC] Local Xray startup is already in progress; deferring handoff until startup completes.',
                    );
                }
                break;
            case 'addUser':
                if (await this.xrayService.isXrayReady()) {
                    await this.handlerService.addUser(action.body);
                } else {
                    this.logger.warn(
                        `[PEER-SYNC] Received addUser but Xray is not running yet. Triggering catch-up...`,
                    );
                    void this.catchUpFromLeader();
                }
                break;
            case 'addUsers':
                if (await this.xrayService.isXrayReady()) {
                    await this.handlerService.addUsers(action.body);
                } else {
                    this.logger.warn(
                        `[PEER-SYNC] Received addUsers but Xray is not running yet. Triggering catch-up...`,
                    );
                    void this.catchUpFromLeader();
                }
                break;
            case 'removeUser':
                if (await this.xrayService.isXrayReady()) {
                    await this.handlerService.removeUser(action.body);
                }
                break;
            case 'removeUsers':
                if (await this.xrayService.isXrayReady()) {
                    await this.handlerService.removeUsers(action.body);
                }
                break;
            case 'suspendUser': {
                const remainingSeconds = Math.max(
                    1,
                    Math.round((action.body.unbanAt - Date.now()) / 1000),
                );
                await this.userSuspensionService.suspendUser(action.body.userId, remainingSeconds, {
                    replicate: false,
                });
                break;
            }
            case 'restoreUser':
                await this.userSuspensionService.restoreUser(action.body.userId, {
                    replicate: false,
                });
                break;
            default:
                this.logger.warn(`Unknown peer sync action: ${JSON.stringify(action)}`);
        }
    }

    /**
     * Export current in-memory Xray configuration and inbounds for new peer startup
     */
    public async exportSnapshot(): Promise<IPeerSnapshotResponse> {
        const xrayConfig = await this.internalService.getXrayConfig();
        const startXrayRequest = this.internalService.getLastStartXrayRequest();
        const xrayUp = await this.xrayService.isXrayReady();
        return { xrayConfig, startXrayRequest, xrayUp };
    }

    private async fetchPeerSnapshots(): Promise<
        Array<{ peerIp: string; snapshot: IPeerSnapshotResponse }>
    > {
        const peerIps = await this.discoverPeerIps();
        const results: Array<{ peerIp: string; snapshot: IPeerSnapshotResponse }> = [];

        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.peerPort}/internal/peer-sync/snapshot`;
                const res = await fetch(url, {
                    headers: { 'X-Secret-Key': this.secretKey },
                    signal: AbortSignal.timeout(3000),
                });
                if (res.ok) {
                    const snapshot = (await res.json()) as IPeerSnapshotResponse;
                    results.push({ peerIp, snapshot });
                }
            } catch (error) {
                this.logger.debug(`Could not fetch snapshot from peer ${peerIp}: ${error}`);
            }
        }

        return results;
    }

    /**
     * Follower discovers leader peer and fetches snapshot
     */
    public async catchUpFromLeader(): Promise<void> {
        if (this.isCatchingUp || this.isShuttingDown) return;
        this.isCatchingUp = true;

        try {
            const peerIps = await this.discoverPeerIps();
            if (peerIps.length === 0) return;

            this.logger.log(
                `[PEER-SYNC] Attempting catch-up sync from peers: ${peerIps.join(', ')}...`,
            );

            for (const peerIp of peerIps) {
                try {
                    const url = `http://${peerIp}:${this.peerPort}/internal/peer-sync/snapshot`;
                    const res = await fetch(url, {
                        headers: { 'X-Secret-Key': this.secretKey },
                        signal: AbortSignal.timeout(5000),
                    });

                    if (res.ok) {
                        const data = (await res.json()) as IPeerSnapshotResponse;
                        if (data.startXrayRequest) {
                            this.logger.log(
                                `[PEER-SYNC] Successfully retrieved snapshot from ${peerIp}. Starting local Xray on follower...`,
                            );
                            this.internalService.setLastStartXrayRequest(data.startXrayRequest);

                            if (!this.xrayService.isStarting) {
                                await this.xrayService.startXray(
                                    data.startXrayRequest,
                                    '127.0.0.1',
                                );
                            }

                            if (await this.xrayService.isXrayReady()) {
                                await this.onLocalXrayBecameReady();
                            }
                            return;
                        } else if (data.xrayConfig && Object.keys(data.xrayConfig).length > 0) {
                            this.logger.log(
                                `[PEER-SYNC] Successfully retrieved config snapshot from ${peerIp}`,
                            );
                            this.internalService.setXrayConfig(data.xrayConfig);
                            return;
                        }
                    }
                } catch (error) {
                    this.logger.debug(`Could not catch up from peer ${peerIp}: ${error}`);
                }
            }
        } finally {
            this.isCatchingUp = false;
        }
    }

    private getResolvedServiceHostname(): string {
        if (!this.headlessService) return '';
        if (this.headlessService.includes('.')) {
            return this.headlessService;
        }
        return `${this.headlessService}.${this.namespace}.svc.cluster.local`;
    }

    private async discoverPeerIps(): Promise<string[]> {
        if (!this.headlessService) return [];

        const hostname = this.getResolvedServiceHostname();
        let addresses: string[] = [];

        try {
            addresses = await dns.resolve4(hostname);
        } catch (error) {
            this.logger.debug(
                `[PEER-SYNC] dns.resolve4 failed for "${hostname}": ${error}. Trying dns.lookup...`,
            );
            try {
                const lookupResults = await dns.lookup(this.headlessService, {
                    all: true,
                    family: 4,
                });
                addresses = lookupResults.map((r) => r.address);
            } catch (lookupError) {
                this.logger.warn(
                    `[PEER-SYNC] Failed to resolve peer addresses for "${hostname}": ${lookupError}`,
                );
                return [];
            }
        }

        const localIps = new Set<string>();
        const ifaces = os.networkInterfaces();
        for (const iface of Object.values(ifaces) as (os.NetworkInterfaceInfo[] | undefined)[]) {
            if (!iface) continue;
            for (const info of iface) {
                localIps.add(info.address);
            }
        }

        return addresses.filter((ip) => !localIps.has(ip));
    }

    /**
     * When this follower pod is fully initialized with running Xray (Hot Standby),
     * request leadership handoff from the existing leader if this pod is newer (e.g. rollout restart).
     */
    public async requestLeadershipHandoff(targetPeerIp?: string, attempt = 1): Promise<void> {
        const maxAttempts = 3;

        if (this.hasRequestedYield || this.leaderElectionService.isLeader || this.isShuttingDown) {
            return;
        }

        // Do not request leadership handoff if this container restarted due to OOM/crash
        if (this.leaderElectionService.containerRestartCount > 0) {
            this.logger.debug(
                `[PEER-SYNC] Skipping leadership handoff request because container has restartCount=${this.leaderElectionService.containerRestartCount} (remaining hot-standby).`,
            );
            this.hasRequestedYield = true;
            return;
        }

        // Never request leadership handoff unless local Xray is verified online and running
        const xrayReady = await this.xrayService.isXrayReady();
        if (!xrayReady) {
            this.logger.debug(
                '[PEER-SYNC] Deferring leadership handoff request because local Xray is not ready yet.',
            );
            return;
        }

        const peerIps = targetPeerIp ? [targetPeerIp] : await this.discoverPeerIps();
        if (peerIps.length === 0) return;

        this.hasRequestedYield = true;
        this.logger.log(
            `[PEER-SYNC] Requesting graceful leadership handoff from leader(s): ${peerIps.join(', ')} (attempt ${attempt}/${maxAttempts})...`,
        );

        let shouldRetry = false;

        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.peerPort}/internal/leader/yield`;
                const res = await fetch(url, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'X-Secret-Key': this.secretKey,
                    },
                    body: JSON.stringify({
                        candidatePod: this.leaderElectionService.identity,
                        candidateBootTime: this.leaderElectionService.startedAt,
                        candidateRestartCount: this.leaderElectionService.containerRestartCount,
                        candidateXrayReady: true,
                    }),
                    signal: AbortSignal.timeout(5000),
                });

                if (res.ok || res.status === 409) {
                    const data = (await res.json()) as {
                        success: boolean;
                        reason?: string;
                        retryable?: boolean;
                    };
                    if (data.success && data.reason !== 'already_follower') {
                        this.logger.log(
                            `[PEER-SYNC] 🏆 Leader ${peerIp} yielded leadership. Triggering immediate election cycle to claim lease...`,
                        );
                        await this.leaderElectionService.triggerElectionNow();
                        return;
                    }

                    if (data.reason === 'already_follower') {
                        continue;
                    }

                    if (
                        data.retryable === false ||
                        data.reason === 'candidate_not_newer' ||
                        data.reason === 'candidate_restarted'
                    ) {
                        this.logger.log(
                            `[PEER-SYNC] Leader ${peerIp} rejected yield permanently (${data.reason}). Remaining in hot-standby mode.`,
                        );
                        return;
                    }

                    this.logger.warn(
                        `[PEER-SYNC] Leader ${peerIp} did not yield yet (${data.reason}).`,
                    );
                    shouldRetry = true;
                }
            } catch (error) {
                this.logger.debug(
                    `[PEER-SYNC] Failed to reach peer ${peerIp} for leadership yield: ${error}`,
                );
                shouldRetry = true;
            }
        }

        if (shouldRetry && attempt < maxAttempts) {
            this.logger.warn(
                `[PEER-SYNC] Scheduling leadership handoff retry ${attempt + 1}/${maxAttempts} in 3s...`,
            );
            this.scheduleYieldRetry(attempt + 1);
        }
    }

    private scheduleYieldRetry(nextAttempt: number): void {
        if (this.yieldRetryTimer) {
            clearTimeout(this.yieldRetryTimer);
        }
        this.yieldRetryTimer = setTimeout(() => {
            this.yieldRetryTimer = null;
            if (!this.leaderElectionService.isLeader && !this.isShuttingDown) {
                this.hasRequestedYield = false;
                // Always re-discover live peer IPs on retry instead of reusing a potentially terminated pod IP
                void this.requestLeadershipHandoff(undefined, nextAttempt);
            }
        }, 3000);
    }
}
