import dns from 'node:dns/promises';
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

import {
    AddUserRequestDto,
    AddUsersRequestDto,
    RemoveUserRequestDto,
    RemoveUsersRequestDto,
} from '../handler/dtos';
import { HandlerService } from '../handler/handler.service';
import { InternalService } from '../internal/internal.service';
import { LeaderElectionService } from '../leader-election/leader-election.service';
import { XrayService } from '../xray-core/xray.service';

export type TPeerReplicationAction =
    | { type: 'startXray'; body: StartXrayCommand.Request }
    | { type: 'addUser'; body: AddUserRequestDto }
    | { type: 'addUsers'; body: AddUsersRequestDto }
    | { type: 'removeUser'; body: RemoveUserRequestDto }
    | { type: 'removeUsers'; body: RemoveUsersRequestDto };

@Injectable()
export class PeerSyncService implements OnApplicationBootstrap, OnApplicationShutdown {
    private readonly logger = new Logger(PeerSyncService.name);
    private readonly headlessService: string | undefined;
    private readonly nodePort: number;
    private readonly peerPort: number;
    private readonly secretKey: string;

    private catchUpTimer: NodeJS.Timeout | null = null;
    private isShuttingDown = false;
    private isCatchingUp = false;

    constructor(
        private readonly configService: TypedConfigService,
        private readonly leaderElectionService: LeaderElectionService,
        private readonly internalService: InternalService,
        @Inject(forwardRef(() => XrayService))
        private readonly xrayService: XrayService,
        @Inject(forwardRef(() => HandlerService))
        private readonly handlerService: HandlerService,
        private readonly commandBus: CommandBus,
    ) {
        this.headlessService = this.configService.get('PEER_HEADLESS_SERVICE');
        this.nodePort = this.configService.getOrThrow('NODE_PORT');
        this.peerPort = this.configService.get('HEALTH_PORT') || 3000;
        this.secretKey = this.configService.getOrThrow('SECRET_KEY');

        if (!this.headlessService) {
            this.logger.log('PEER_HEADLESS_SERVICE is not configured. Standalone mode active.');
        }
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
    }

    /**
     * Periodically check if follower needs to catch up (e.g. if leader started after follower or Xray is not yet running)
     */
    private startFollowerCatchUpLoop(): void {
        if (this.catchUpTimer) return;

        this.catchUpTimer = setInterval(() => {
            if (this.isShuttingDown) return;

            // Leaders do not need to poll followers
            if (this.leaderElectionService.isLeader) {
                return;
            }

            // If Xray on Standby replica is not yet online, keep trying to catch up from Leader
            if (!this.xrayService.isOnline && !this.isCatchingUp) {
                void this.catchUpFromLeader();
            }
        }, 2500);
    }

    /**
     * Invoked when this pod is promoted to Leader
     */
    public async onPromotedToLeader(): Promise<void> {
        this.logger.log('[PEER-SYNC] Pod promoted to LEADER. Ensuring local Xray state...');

        if (!this.xrayService.isOnline) {
            const cachedRequest = this.internalService.getLastStartXrayRequest();
            if (cachedRequest) {
                this.logger.log(
                    '[PEER-SYNC] Starting Xray on newly promoted leader using stored configuration...',
                );
                await this.xrayService.startXray(cachedRequest, '127.0.0.1');
            } else {
                this.logger.warn(
                    '[PEER-SYNC] Promoted to leader but no cached Xray config available. Waiting for panel command.',
                );
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
                await this.xrayService.startXray(action.body, '127.0.0.1');
                break;
            case 'addUser':
                if (this.xrayService.isOnline) {
                    await this.handlerService.addUser(action.body);
                } else {
                    this.logger.warn(
                        `[PEER-SYNC] Received addUser but Xray is not running yet. Triggering catch-up...`,
                    );
                    void this.catchUpFromLeader();
                }
                break;
            case 'addUsers':
                if (this.xrayService.isOnline) {
                    await this.handlerService.addUsers(action.body);
                } else {
                    this.logger.warn(
                        `[PEER-SYNC] Received addUsers but Xray is not running yet. Triggering catch-up...`,
                    );
                    void this.catchUpFromLeader();
                }
                break;
            case 'removeUser':
                if (this.xrayService.isOnline) {
                    await this.handlerService.removeUser(action.body);
                }
                break;
            case 'removeUsers':
                if (this.xrayService.isOnline) {
                    await this.handlerService.removeUsers(action.body);
                }
                break;
            default:
                this.logger.warn(`Unknown peer sync action: ${JSON.stringify(action)}`);
        }
    }

    /**
     * Export current in-memory Xray configuration and inbounds for new peer startup
     */
    public async exportSnapshot(): Promise<{
        xrayConfig: Record<string, unknown> | null;
        startXrayRequest: StartXrayCommand.Request | null;
    }> {
        const xrayConfig = await this.internalService.getXrayConfig();
        const startXrayRequest = this.internalService.getLastStartXrayRequest();
        return { xrayConfig, startXrayRequest };
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
                        const data = (await res.json()) as {
                            xrayConfig: Record<string, unknown> | null;
                            startXrayRequest: StartXrayCommand.Request | null;
                        };
                        if (data.startXrayRequest) {
                            this.logger.log(
                                `[PEER-SYNC] Successfully retrieved snapshot from ${peerIp}. Starting local Xray on follower...`,
                            );
                            await this.xrayService.startXray(data.startXrayRequest, '127.0.0.1');
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

    private async discoverPeerIps(): Promise<string[]> {
        if (!this.headlessService) return [];

        try {
            const addresses = await dns.resolve4(this.headlessService);
            const localIps = new Set<string>();

            const ifaces = os.networkInterfaces();
            for (const iface of Object.values(ifaces) as (
                | os.NetworkInterfaceInfo[]
                | undefined
            )[]) {
                if (!iface) continue;
                for (const info of iface) {
                    localIps.add(info.address);
                }
            }

            return addresses.filter((ip) => !localIps.has(ip));
        } catch {
            return [];
        }
    }
}
