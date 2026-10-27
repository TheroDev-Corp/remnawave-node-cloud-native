import { describe, expect, it, mock } from 'bun:test';

import { HealthController } from '../src/modules/health/health.controller';
import { LeaderElectionService } from '../src/modules/leader-election/leader-election.service';
import { PeerSyncService } from '../src/modules/peer-sync/peer-sync.service';

describe('HealthController Probes', () => {
    it('should return 200 OK for Readiness probe when node is Leader', async () => {
        const mockLeaderElection = {
            isLeader: true,
            isServingTraffic: true,
            isDraining: false,
            identity: 'remnanode-pod-0',
        };
        const mockXrayProcess = {
            getStatus: async () => ({ up: true, pid: 1234, raw: 'up' }),
        };

        const controller = new HealthController(mockLeaderElection as any, mockXrayProcess as any);

        let statusCode = 0;
        let responseBody: any = null;

        const res: any = {
            status: (code: number) => {
                statusCode = code;
                return {
                    json: (data: any) => {
                        responseBody = data;
                    },
                };
            },
        };

        await controller.ready(res);

        expect(statusCode).toBe(200);
        expect(responseBody.role).toBe('leader');
        expect(responseBody.servingTraffic).toBe(true);
        expect(responseBody.xray).toBe('up');
        expect(responseBody.status).toBe('ok');
    });

    it('should return 200 OK with standby role for Readiness probe when node is Standby/Follower', async () => {
        const mockLeaderElection = {
            isLeader: false,
            isServingTraffic: false,
            isDraining: false,
            identity: 'remnanode-pod-1',
        };
        const mockXrayProcess = {
            getStatus: async () => ({ up: true, pid: 5678, raw: 'up' }),
        };

        const controller = new HealthController(mockLeaderElection as any, mockXrayProcess as any);

        let statusCode = 0;
        let responseBody: any = null;

        const res: any = {
            status: (code: number) => {
                statusCode = code;
                return {
                    json: (data: any) => {
                        responseBody = data;
                    },
                };
            },
        };

        await controller.ready(res);

        expect(statusCode).toBe(200);
        expect(responseBody.role).toBe('standby');
        expect(responseBody.servingTraffic).toBe(false);
        expect(responseBody.status).toBe('ok');
    });

    it('should return 200 OK for Liveness probe', async () => {
        const mockLeaderElection = { isLeader: false, identity: 'test-pod' };
        const mockXrayProcess = {
            getStatus: async () => ({ up: true, pid: 9999, raw: 'up' }),
        };

        const controller = new HealthController(mockLeaderElection as any, mockXrayProcess as any);

        let statusCode = 0;
        let responseBody: any = null;

        const res: any = {
            status: (code: number) => {
                statusCode = code;
                return {
                    json: (data: any) => {
                        responseBody = data;
                    },
                };
            },
        };

        await controller.live(res);

        expect(statusCode).toBe(200);
        expect(responseBody.status).toBe('ok');
        expect(responseBody.role).toBe('follower');
        expect(responseBody.identity).toBe('test-pod');
    });
});

describe('LeaderElectionService HA & OOM Failover', () => {
    function createService() {
        const configService = {
            getOrThrow: (key: string) => {
                if (key === 'K8S_LEADER_ELECTION_ENABLED') return true;
                if (key === 'K8S_LEASE_NAME') return 'remnanode-leader';
                return '';
            },
        };
        const publishedEvents: any[] = [];
        const eventBus = {
            publish: (ev: any) => publishedEvents.push(ev),
        };

        process.env.POD_NAME = 'remnanode-pod-standby';
        process.env.POD_NAMESPACE = 'remnanode';

        const service = new LeaderElectionService(configService as any, eventBus as any);
        return { service, publishedEvents };
    }

    it('should block lease acquisition when local Xray is not ready and peer has active Xray', async () => {
        const { service } = createService();

        const patchedPods: Array<{ name: string; labels: Record<string, string | null> }> = [];
        let updateLeaseCalled = false;

        (service as any).client = {
            getLease: async () => ({
                status: 200,
                lease: {
                    apiVersion: 'coordination.k8s.io/v1',
                    kind: 'Lease',
                    metadata: { name: 'remnanode-leader', namespace: 'remnanode' },
                    spec: {
                        holderIdentity: 'remnanode-pod-old',
                        renewTime: new Date(Date.now() - 30_000).toISOString(),
                        leaseDurationSeconds: 10,
                    },
                },
            }),
            updateLease: async () => {
                updateLeaseCalled = true;
                return { status: 200, lease: {} };
            },
            patchPodLabels: async (_ns: string, name: string, labels: any) => {
                patchedPods.push({ name, labels });
                return { status: 200 };
            },
        };

        service.registerHealthGate({
            canAcquireLeadership: async () => false,
            checkLeaderXrayHealth: async () => ({ healthy: true, hasHealthyPeer: false }),
            notifyStandbyToPromote: async () => {},
        });

        await (service as any).electionLoop();

        expect(updateLeaseCalled).toBe(false);
        expect(service.isLeader).toBe(false);
        // Should still ensure its own standby label is applied
        expect(patchedPods).toEqual([
            { name: 'remnanode-pod-standby', labels: { role: 'standby' } },
        ]);
    });

    it('should acquire expired lease when Xray is ready, label itself leader, and clear stale leader label on OOMed pod', async () => {
        const { service } = createService();

        const patchedPods: Array<{ name: string; labels: Record<string, string | null> }> = [];

        (service as any).client = {
            getLease: async () => ({
                status: 200,
                lease: {
                    apiVersion: 'coordination.k8s.io/v1',
                    kind: 'Lease',
                    metadata: {
                        name: 'remnanode-leader',
                        namespace: 'remnanode',
                        resourceVersion: '10',
                    },
                    spec: {
                        holderIdentity: 'remnanode-pod-oomed',
                        renewTime: new Date(Date.now() - 30_000).toISOString(),
                        leaseDurationSeconds: 10,
                        leaseTransitions: 2,
                    },
                },
            }),
            updateLease: async (_ns: string, _name: string, updated: any) => ({
                status: 200,
                lease: updated,
            }),
            patchPodLabels: async (_ns: string, name: string, labels: any) => {
                patchedPods.push({ name, labels });
                return { status: 200 };
            },
        };

        service.registerHealthGate({
            canAcquireLeadership: async () => true,
            checkLeaderXrayHealth: async () => ({ healthy: true, hasHealthyPeer: false }),
            notifyStandbyToPromote: async () => {},
        });

        await (service as any).electionLoop();

        expect(service.isLeader).toBe(true);
        expect(patchedPods).toContainEqual({
            name: 'remnanode-pod-standby',
            labels: { role: 'leader' },
        });
        expect(patchedPods).toContainEqual({
            name: 'remnanode-pod-oomed',
            labels: { role: 'standby' },
        });
    });

    it('should immediately step down and notify standby when local Xray dies (in-container OOM) while standby is healthy', async () => {
        const { service } = createService();
        (service as any).leaderState = true;
        (service as any).appliedRoleLabel = 'leader';

        let leaseReleased = false;
        let standbyNotified = false;
        const patchedPods: Array<{ name: string; labels: Record<string, string | null> }> = [];

        (service as any).client = {
            getLease: async () => ({
                status: 200,
                lease: {
                    apiVersion: 'coordination.k8s.io/v1',
                    kind: 'Lease',
                    metadata: { name: 'remnanode-leader', namespace: 'remnanode' },
                    spec: {
                        holderIdentity: 'remnanode-pod-standby',
                        renewTime: new Date().toISOString(),
                        leaseDurationSeconds: 10,
                    },
                },
            }),
            updateLease: async (_ns: string, _name: string, updated: any) => {
                if (updated.spec.holderIdentity === null) {
                    leaseReleased = true;
                }
                return { status: 200, lease: updated };
            },
            patchPodLabels: async (_ns: string, name: string, labels: any) => {
                patchedPods.push({ name, labels });
                return { status: 200 };
            },
        };

        service.registerHealthGate({
            canAcquireLeadership: async () => false,
            checkLeaderXrayHealth: async () => ({ healthy: false, hasHealthyPeer: true }),
            notifyStandbyToPromote: async () => {
                standbyNotified = true;
            },
        });

        await (service as any).electionLoop();

        expect(service.isLeader).toBe(false);
        expect(leaseReleased).toBe(true);
        expect(standbyNotified).toBe(true);
        expect(patchedPods).toContainEqual({
            name: 'remnanode-pod-standby',
            labels: { role: 'standby' },
        });
    });

    it('should reject yieldLeadership when candidate has restartCount > 0 (OOM restart) or Xray not ready', async () => {
        const { service } = createService();
        (service as any).leaderState = true;
        (service as any).podCreationTime = 1000;

        const notReadyRes = await service.yieldLeadership('pod-new', 2000, 0, false);
        expect(notReadyRes.success).toBe(false);
        expect(notReadyRes.reason).toBe('candidate_xray_not_ready');
        expect(notReadyRes.retryable).toBe(true);

        const restartedRes = await service.yieldLeadership('pod-oom-restarted', 2000, 3, true);
        expect(restartedRes.success).toBe(false);
        expect(restartedRes.reason).toBe('candidate_restarted');
        expect(restartedRes.retryable).toBe(false);

        const olderRes = await service.yieldLeadership('pod-older', 500, 0, true);
        expect(olderRes.success).toBe(false);
        expect(olderRes.reason).toBe('candidate_not_newer');
        expect(olderRes.retryable).toBe(false);
    });
});

describe('PeerSyncService Readiness & Handoff Guards', () => {
    it('should not request leadership handoff when applyReplicatedAction(startXray) sees Xray is still starting', async () => {
        let registeredGate: any = null;
        const mockConfig = {
            get: (k: string) => (k === 'PEER_HEADLESS_SERVICE' ? 'remnanode-headless' : 3000),
            getOrThrow: () => 'secret',
        };
        const mockLeaderElection = {
            isLeader: false,
            identity: 'pod-2',
            startedAt: 2000,
            containerRestartCount: 0,
            registerHealthGate: (gate: any) => {
                registeredGate = gate;
            },
            triggerElectionNow: mock(async () => {}),
        };
        const mockInternal = {
            setLastStartXrayRequest: mock(() => {}),
            getLastStartXrayRequest: () => null,
        };
        const mockXray = {
            isStarting: true,
            isXrayReady: async () => false,
            startXray: mock(async () => {}),
        };

        const peerSync = new PeerSyncService(
            mockConfig as any,
            mockLeaderElection as any,
            mockInternal as any,
            mockXray as any,
            {} as any,
            {} as any,
            {} as any,
        );

        expect(registeredGate).toBe(peerSync);

        const handoffSpy = mock(async () => {});
        peerSync.requestLeadershipHandoff = handoffSpy;

        await peerSync.applyReplicatedAction({
            type: 'startXray',
            body: { xrayConfig: {}, internals: {} } as any,
        });

        expect(mockXray.startXray).not.toHaveBeenCalled();
        expect(handoffSpy).not.toHaveBeenCalled();
    });
});
