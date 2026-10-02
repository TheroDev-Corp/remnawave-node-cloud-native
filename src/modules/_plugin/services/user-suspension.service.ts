import { forwardRef, Inject, Injectable, Logger, OnModuleDestroy } from '@nestjs/common';

import { XtlsApi } from '@remnawave/xtls-sdk';
import { InjectXtls } from '@remnawave/xtls-sdk-nestjs';

import { AddUserRequestDto } from '../../handler/dtos';
import { HandlerService } from '../../handler/handler.service';
import { InternalService } from '../../internal/internal.service';
import { PeerSyncService } from '../../peer-sync/peer-sync.service';

interface ISuspendedUserState {
    timer: NodeJS.Timeout;
    unbanAt: number;
    userDef?: AddUserRequestDto;
}

@Injectable()
export class UserSuspensionService implements OnModuleDestroy {
    private readonly logger = new Logger(UserSuspensionService.name);
    private readonly suspendedUsers = new Map<string, ISuspendedUserState>();

    constructor(
        @Inject(forwardRef(() => HandlerService))
        private readonly handlerService: HandlerService,
        private readonly internalService: InternalService,
        @InjectXtls() private readonly xtlsApi: XtlsApi,
        @Inject(forwardRef(() => PeerSyncService))
        private readonly peerSyncService: PeerSyncService,
    ) {}

    onModuleDestroy(): void {
        for (const [userId, state] of this.suspendedUsers) {
            clearTimeout(state.timer);
            this.logger.debug(`Cleared suspension timer for user: ${userId}`);
        }
        this.suspendedUsers.clear();
    }

    public isUserSuspended(userId: string): boolean {
        return this.suspendedUsers.has(userId);
    }

    public getSuspendedUsers(): Array<{
        userId: string;
        unbanAt: number;
        remainingSeconds: number;
    }> {
        const now = Date.now();
        const result: Array<{ userId: string; unbanAt: number; remainingSeconds: number }> = [];

        for (const [userId, state] of this.suspendedUsers) {
            result.push({
                userId,
                unbanAt: state.unbanAt,
                remainingSeconds: Math.max(0, Math.round((state.unbanAt - now) / 1000)),
            });
        }

        return result;
    }

    /**
     * Temporarily suspends user from all Xray inbounds for `durationSeconds`
     */
    public async suspendUser(
        userId: string,
        durationSeconds: number,
        options?: { replicate?: boolean },
    ): Promise<boolean> {
        try {
            const existing = this.suspendedUsers.get(userId);
            if (existing) {
                clearTimeout(existing.timer);
            }

            const cachedDef = this.internalService.getUserDefinition(userId);

            this.logger.warn(
                `[SUSPENSION] Suspending user "${userId}" for ${durationSeconds}s from all Xray inbounds...`,
            );

            // Remove from active Xray inbounds
            const inboundTags = this.internalService.getXtlsConfigInbounds();
            for (const tag of inboundTags) {
                try {
                    await this.xtlsApi.handler.removeUser(tag, userId);
                } catch (error) {
                    this.logger.debug(`Could not remove user ${userId} from tag ${tag}: ${error}`);
                }
            }

            const unbanAt = Date.now() + durationSeconds * 1000;
            const timer = setTimeout(() => {
                void this.restoreUser(userId);
            }, durationSeconds * 1000);

            this.suspendedUsers.set(userId, {
                timer,
                unbanAt,
                userDef: cachedDef,
            });

            if (options?.replicate !== false) {
                void this.peerSyncService.replicateToPeers({
                    type: 'suspendUser',
                    body: { userId, durationSeconds, unbanAt },
                });
            }

            return true;
        } catch (error) {
            this.logger.error(`Failed to suspend user "${userId}": ${error}`);
            return false;
        }
    }

    /**
     * Restores suspended user back into Xray inbounds
     */
    public async restoreUser(userId: string, options?: { replicate?: boolean }): Promise<boolean> {
        try {
            const state = this.suspendedUsers.get(userId);
            if (state) {
                clearTimeout(state.timer);
                this.suspendedUsers.delete(userId);
            }

            const userDef = state?.userDef ?? this.internalService.getUserDefinition(userId);
            if (!userDef) {
                this.logger.warn(
                    `[SUSPENSION] No user definition found to restore user "${userId}". User might need panel sync.`,
                );
                return false;
            }

            this.logger.log(`[SUSPENSION] Restoring user "${userId}" back into Xray inbounds...`);
            await this.handlerService.addUser(userDef);

            if (options?.replicate !== false) {
                void this.peerSyncService.replicateToPeers({
                    type: 'restoreUser',
                    body: { userId },
                });
            }

            return true;
        } catch (error) {
            this.logger.error(`Failed to restore user "${userId}": ${error}`);
            return false;
        }
    }
}
