import fs from 'node:fs';
import os from 'node:os';

import { Injectable, Logger, OnApplicationBootstrap, OnApplicationShutdown } from '@nestjs/common';
import { EventBus } from '@nestjs/cqrs';

import { TypedConfigService } from '@common/config/app-config';

import { LeaderDemotedEvent } from './events/leader-demoted.event';
import { LeaderPromotedEvent } from './events/leader-promoted.event';
import { IK8sLease, K8sLeaseClient } from './k8s-lease.client';

function formatK8sMicroTime(date = new Date()): string {
    return date.toISOString().replace(/\.(\d{3})Z$/, '.$1000Z');
}

@Injectable()
export class LeaderElectionService implements OnApplicationBootstrap, OnApplicationShutdown {
    private readonly logger = new Logger(LeaderElectionService.name);
    private readonly enabled: boolean;
    private readonly leaseName: string;
    private readonly podName: string;
    private readonly namespace: string;
    private readonly leaseDurationSeconds = 10;
    private readonly renewIntervalMs = 2000;

    private readonly client: K8sLeaseClient;
    private readonly bootTime = Date.now();
    private leaderSince = 0;
    private isDrainingState = false;
    private drainTimer: NodeJS.Timeout | null = null;
    private readonly minLeaderTenureMs = 30_000;
    private readonly drainDurationMs = 10_000;

    private suppressAcquisitionUntil = 0;
    private leaderState = false;
    private currentResourceVersion: string | null = null;
    private loopTimer: NodeJS.Timeout | null = null;
    private isShuttingDown = false;

    constructor(
        private readonly configService: TypedConfigService,
        private readonly eventBus: EventBus,
    ) {
        this.enabled = this.configService.getOrThrow('K8S_LEADER_ELECTION_ENABLED');
        this.leaseName = this.configService.getOrThrow('K8S_LEASE_NAME');

        this.podName =
            process.env.POD_NAME ??
            process.env.HOSTNAME ??
            os.hostname() ??
            `node-${Math.random().toString(36).substring(2, 8)}`;

        const nsFile = '/var/run/secrets/kubernetes.io/serviceaccount/namespace';
        this.namespace =
            process.env.POD_NAMESPACE ??
            (fs.existsSync(nsFile) ? fs.readFileSync(nsFile, 'utf8').trim() : 'remnanode');

        this.client = new K8sLeaseClient();

        // Default to leader if election is disabled (single-node mode)
        if (!this.enabled) {
            this.leaderState = true;
        }
    }

    public get isLeader(): boolean {
        return this.leaderState;
    }

    public get identity(): string {
        return this.podName;
    }

    public get isElectionEnabled(): boolean {
        return this.enabled;
    }

    public get startedAt(): number {
        return this.bootTime;
    }

    public get isDraining(): boolean {
        return this.isDrainingState;
    }

    public get isServingTraffic(): boolean {
        return this.leaderState || this.isDrainingState;
    }

    async onApplicationBootstrap(): Promise<void> {
        if (!this.enabled) {
            this.logger.log('K8s Leader Election disabled, running as standalone Leader.');
            return;
        }

        this.logger.log(
            `Starting K8s Leader Election (pod: ${this.podName}, namespace: ${this.namespace}, lease: ${this.leaseName})...`,
        );

        // Immediate first attempt
        await this.electionLoop();

        // Start periodic loop
        this.loopTimer = setInterval(() => {
            void this.electionLoop();
        }, this.renewIntervalMs);
    }

    async onApplicationShutdown(signal?: string): Promise<void> {
        this.isShuttingDown = true;
        if (this.loopTimer) {
            clearInterval(this.loopTimer);
            this.loopTimer = null;
        }
        if (this.drainTimer) {
            clearTimeout(this.drainTimer);
            this.drainTimer = null;
        }

        if (this.leaderState && this.enabled) {
            this.logger.log(`Releasing lease "${this.leaseName}" due to shutdown (${signal})...`);
            await this.releaseLease();
            this.setLeader(false);

            // Graceful connection drain before exit without needing preStop sleep
            this.logger.log(`Gracefully draining connections for 5s before exit...`);
            await new Promise((resolve) => setTimeout(resolve, 5000));
        }
    }

    private async electionLoop(): Promise<void> {
        if (this.isShuttingDown) return;
        if (!this.leaderState && Date.now() < this.suppressAcquisitionUntil) {
            return;
        }

        try {
            const { status, lease } = await this.client.getLease(this.namespace, this.leaseName);

            if (status === 404) {
                // Lease does not exist -> try to create and acquire
                await this.tryCreateAndAcquire();
                return;
            }

            if (status === 200 && lease) {
                this.currentResourceVersion = lease.metadata.resourceVersion ?? null;
                const holder = lease.spec.holderIdentity;
                const renewTimeStr = lease.spec.renewTime;
                const duration = lease.spec.leaseDurationSeconds ?? this.leaseDurationSeconds;

                const isCurrentHolder = holder === this.podName;
                const isExpired = this.isLeaseExpired(renewTimeStr, duration);

                if (isCurrentHolder) {
                    // We are current leader -> renew
                    await this.tryRenew(lease);
                    if (!this.leaderState) {
                        this.setLeader(true);
                    }
                } else if (!holder || isExpired) {
                    // Lease is expired or vacant -> try acquire
                    this.logger.log(
                        `Lease "${this.leaseName}" expired (previous holder: ${holder}), attempting acquisition...`,
                    );
                    await this.tryAcquire(lease);
                } else {
                    // Lease held by someone else and still valid
                    if (this.leaderState) {
                        this.logger.warn(
                            `Lost lease "${this.leaseName}" to ${holder}. Demoting to follower.`,
                        );
                        this.setLeader(false);
                    }
                }
            }
        } catch (error) {
            this.logger.warn(
                `Leader election cycle failed: ${error instanceof Error ? error.message : String(error)}`,
            );
        }
    }

    private async tryCreateAndAcquire(): Promise<void> {
        const now = formatK8sMicroTime();
        const leasePayload: IK8sLease = {
            apiVersion: 'coordination.k8s.io/v1',
            kind: 'Lease',
            metadata: {
                name: this.leaseName,
                namespace: this.namespace,
            },
            spec: {
                holderIdentity: this.podName,
                leaseDurationSeconds: this.leaseDurationSeconds,
                acquireTime: now,
                renewTime: now,
                leaseTransitions: 1,
            },
        };

        const res = await this.client.createLease(this.namespace, leasePayload);
        if (res.status === 201 && res.lease) {
            this.currentResourceVersion = res.lease.metadata.resourceVersion ?? null;
            this.setLeader(true);
        }
    }

    private async tryRenew(lease: IK8sLease): Promise<void> {
        const now = formatK8sMicroTime();
        const updatedLease: IK8sLease = {
            ...lease,
            spec: {
                ...lease.spec,
                holderIdentity: this.podName,
                renewTime: now,
                leaseDurationSeconds: this.leaseDurationSeconds,
            },
        };

        const res = await this.client.updateLease(this.namespace, this.leaseName, updatedLease);
        if (res.status === 200 && res.lease) {
            this.currentResourceVersion = res.lease.metadata.resourceVersion ?? null;
        } else if (res.status === 409) {
            // Conflict -> someone else updated or acquired
            this.currentResourceVersion = null;
        }
    }

    private async tryAcquire(lease: IK8sLease): Promise<void> {
        const now = formatK8sMicroTime();
        const transitions = (lease.spec.leaseTransitions ?? 0) + 1;
        const updatedLease: IK8sLease = {
            ...lease,
            spec: {
                ...lease.spec,
                holderIdentity: this.podName,
                acquireTime: now,
                renewTime: now,
                leaseDurationSeconds: this.leaseDurationSeconds,
                leaseTransitions: transitions,
            },
        };

        const res = await this.client.updateLease(this.namespace, this.leaseName, updatedLease);
        if (res.status === 200 && res.lease) {
            this.currentResourceVersion = res.lease.metadata.resourceVersion ?? null;
            this.setLeader(true);
        }
    }

    private async releaseLease(): Promise<void> {
        try {
            const { status, lease } = await this.client.getLease(this.namespace, this.leaseName);
            if (status === 200 && lease && lease.spec.holderIdentity === this.podName) {
                const releasedLease: IK8sLease = {
                    ...lease,
                    spec: {
                        ...lease.spec,
                        holderIdentity: null,
                    },
                };
                await this.client.updateLease(this.namespace, this.leaseName, releasedLease);
            }
        } catch (error) {
            this.logger.warn(`Failed to release lease on shutdown: ${error}`);
        }
    }

    private isLeaseExpired(renewTimeStr?: string, durationSeconds = 10): boolean {
        if (!renewTimeStr) return true;
        const renewTime = new Date(renewTimeStr).getTime();
        return Date.now() > renewTime + durationSeconds * 1000;
    }

    private setLeader(leader: boolean): void {
        const previous = this.leaderState;
        this.leaderState = leader;

        if (!previous && leader) {
            this.leaderSince = Date.now();
            this.isDrainingState = false;
            if (this.drainTimer) {
                clearTimeout(this.drainTimer);
                this.drainTimer = null;
            }
            this.logger.log(`🏆 Pod "${this.podName}" promoted to LEADER.`);
            void this.updatePodRoleLabel('leader');
            this.eventBus.publish(new LeaderPromotedEvent(this.podName));
        } else if (previous && !leader) {
            this.logger.warn(`Pod "${this.podName}" demoted to FOLLOWER / STANDBY.`);
            void this.updatePodRoleLabel('standby');
            this.eventBus.publish(new LeaderDemotedEvent(this.podName));
        }
    }

    public async updatePodRoleLabel(role: 'leader' | 'standby'): Promise<void> {
        if (!this.podName || !this.namespace) return;
        try {
            const res = await this.client.patchPodLabels(this.namespace, this.podName, {
                role,
            });
            if (res.status === 200) {
                this.logger.log(
                    `🏷️ Successfully labeled pod "${this.podName}" with role="${role}"`,
                );
            } else {
                this.logger.warn(
                    `Failed to patch pod label role="${role}" (HTTP ${res.status}): ${res.body}`,
                );
            }
        } catch (error) {
            this.logger.warn(
                `Error patching pod label role="${role}": ${error instanceof Error ? error.message : String(error)}`,
            );
        }
    }

    /**
     * Gracefully yields leadership to a newer candidate pod (e.g. during rolling update)
     */
    public async yieldLeadership(
        candidatePod: string,
        candidateBootTime: number,
    ): Promise<{ success: boolean; reason?: string }> {
        if (!this.leaderState) {
            return { success: true, reason: 'already_follower' };
        }

        if (candidateBootTime <= this.bootTime) {
            this.logger.warn(
                `[LEADER-ELECTION] Rejecting yield request from "${candidatePod}" (candidateBootTime ${candidateBootTime} <= local ${this.bootTime})`,
            );
            return { success: false, reason: 'candidate_not_newer' };
        }

        // Anti-flapping: If candidate is newer, but current leader acquired leadership only recently AND candidate booted BEFORE current leader became leader,
        // it means candidate is NOT a new deployment rollout, but a simultaneous pod.
        // During rolling update, a new deployment pod has candidateBootTime > local this.bootTime, which is genuine rollout.
        // However, if the leader only just acquired the lease from another pod (e.g. during pod death), we still honor yield to a newer pod!
        // We only reject if candidate is NOT newer (already checked above) or if the leader was booted later than candidate.

        this.logger.log(
            `[LEADER-ELECTION] 🤝 Gracefully yielding leadership to newer pod "${candidatePod}". Releasing lease and entering 10s draining overlap...`,
        );

        // Suppress re-acquisition on this pod for 20s so newer pod has ample time to acquire
        this.suppressAcquisitionUntil = Date.now() + 20_000;

        // Release lease immediately so candidate pod claims it at once
        await this.releaseLease();

        // Enter draining state: keep readiness probe 200 OK for 10s so both pods overlap in K8s Service Endpoints!
        this.isDrainingState = true;
        if (this.drainTimer) {
            clearTimeout(this.drainTimer);
        }
        this.drainTimer = setTimeout(() => {
            this.logger.log(
                `[LEADER-ELECTION] Draining period completed. Stepping down to standby.`,
            );
            this.isDrainingState = false;
            this.setLeader(false);
        }, this.drainDurationMs);

        return { success: true };
    }

    /**
     * Immediately triggers an election cycle without waiting for the periodic timer
     */
    public async triggerElectionNow(): Promise<void> {
        if (this.isShuttingDown) return;
        await this.electionLoop();
    }
}
