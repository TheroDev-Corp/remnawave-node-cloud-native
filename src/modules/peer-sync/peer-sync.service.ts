import dns from 'node:dns/promises';
import os from 'node:os';

import { Injectable, Logger, OnApplicationBootstrap } from '@nestjs/common';
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
export class PeerSyncService implements OnApplicationBootstrap {
    private readonly logger = new Logger(PeerSyncService.name);
    private readonly headlessService: string | undefined;
    private readonly nodePort: number;
    private readonly secretKey: string;

    constructor(
        private readonly configService: TypedConfigService,
        private readonly leaderElectionService: LeaderElectionService,
        private readonly internalService: InternalService,
        private readonly xrayService: XrayService,
        private readonly handlerService: HandlerService,
        private readonly commandBus: CommandBus,
    ) {
        this.headlessService = this.configService.getOrThrow('PEER_HEADLESS_SERVICE');
        this.nodePort = this.configService.getOrThrow('NODE_PORT');
        this.secretKey = this.configService.getOrThrow('SECRET_KEY');
    }

    async onApplicationBootstrap(): Promise<void> {
        // If headless service is configured and we are Follower on startup, catch up from Leader
        if (this.headlessService && !this.leaderElectionService.isLeader) {
            await this.catchUpFromLeader();
        }
    }

    /**
     * Called by Leader to replicate an operation to peer Follower pods
     */
    public async replicateToPeers(action: TPeerReplicationAction): Promise<void> {
        if (!this.headlessService) return;

        const peerIps = await this.discoverPeerIps();
        if (peerIps.length === 0) return;

        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.nodePort}/internal/peer-sync/apply`;
                await fetch(url, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'X-Secret-Key': this.secretKey,
                    },
                    body: JSON.stringify(action),
                    signal: AbortSignal.timeout(5000),
                });
                this.logger.debug(`Replicated action "${action.type}" to peer ${peerIp}`);
            } catch (error) {
                this.logger.warn(
                    `Failed to replicate "${action.type}" to peer ${peerIp}: ${error}`,
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
                await this.handlerService.addUser(action.body);
                break;
            case 'addUsers':
                await this.handlerService.addUsers(action.body);
                break;
            case 'removeUser':
                await this.handlerService.removeUser(action.body);
                break;
            case 'removeUsers':
                await this.handlerService.removeUsers(action.body);
                break;
            default:
                this.logger.warn(`Unknown peer sync action: ${JSON.stringify(action)}`);
        }
    }

    /**
     * Export current in-memory Xray configuration and inbounds for new peer startup
     */
    public async exportSnapshot(): Promise<{
        xrayConfig: Record<string, unknown>;
    }> {
        const xrayConfig = await this.internalService.getXrayConfig();
        return { xrayConfig };
    }

    /**
     * Follower discovers leader peer and fetches snapshot
     */
    private async catchUpFromLeader(): Promise<void> {
        const peerIps = await this.discoverPeerIps();
        if (peerIps.length === 0) return;

        this.logger.log(
            `[PEER-SYNC] Attempting catch-up sync from peers: ${peerIps.join(', ')}...`,
        );

        for (const peerIp of peerIps) {
            try {
                const url = `http://${peerIp}:${this.nodePort}/internal/peer-sync/snapshot`;
                const res = await fetch(url, {
                    headers: { 'X-Secret-Key': this.secretKey },
                    signal: AbortSignal.timeout(5000),
                });

                if (res.ok) {
                    const data = (await res.json()) as { xrayConfig: Record<string, unknown> };
                    if (data.xrayConfig && Object.keys(data.xrayConfig).length > 0) {
                        this.logger.log(
                            `[PEER-SYNC] Successfully retrieved snapshot from ${peerIp}`,
                        );
                        // Apply config to Follower's local Xray
                        this.internalService.setXrayConfig(data.xrayConfig);
                        return;
                    }
                }
            } catch (error) {
                this.logger.debug(`Could not catch up from peer ${peerIp}: ${error}`);
            }
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
