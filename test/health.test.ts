import { describe, expect, it } from 'bun:test';

import { HealthController } from '../src/modules/health/health.controller';

describe('HealthController Probes', () => {
    it('should return 200 OK for Readiness probe when node is Leader', async () => {
        const mockLeaderElection = {
            isLeader: true,
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
        expect(responseBody.status).toBe('ok');
    });

    it('should return 503 Service Unavailable for Readiness probe when node is Standby/Follower', async () => {
        const mockLeaderElection = {
            isLeader: false,
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

        expect(statusCode).toBe(503);
        expect(responseBody.role).toBe('follower');
        expect(responseBody.status).toBe('standby');
    });

    it('should return 200 OK for Liveness probe when Xray is running', async () => {
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
        expect(responseBody.xray).toBe('up');
        expect(responseBody.pid).toBe(9999);
    });
});
