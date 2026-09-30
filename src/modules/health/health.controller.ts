import type { Response } from 'express';

import { Controller, Get, HttpStatus, Res } from '@nestjs/common';

import { LeaderElectionService } from '../leader-election/leader-election.service';
import { XrayProcessService } from '../xray-core/xray-process.service';

@Controller('health')
export class HealthController {
    constructor(
        private readonly leaderElectionService: LeaderElectionService,
        private readonly xrayProcessService: XrayProcessService,
    ) {}

    @Get('live')
    async live(@Res() res: Response): Promise<Response> {
        // Liveness probe: returns 200 OK as long as the process is alive.
        // Standby replicas in leader election do not run Xray, so we only check Xray if this node is leader.
        if (this.leaderElectionService.isLeader) {
            try {
                const xrayStatus = await this.xrayProcessService.getStatus();
                if (!xrayStatus.up) {
                    return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
                        status: 'degraded',
                        role: 'leader',
                        xray: 'down',
                    });
                }
            } catch (error) {
                return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
                    status: 'error',
                    message: error instanceof Error ? error.message : String(error),
                });
            }
        }

        return res.status(HttpStatus.OK).json({
            status: 'ok',
            role: this.leaderElectionService.isLeader ? 'leader' : 'follower',
            identity: this.leaderElectionService.identity,
        });
    }

    @Get('ready')
    async ready(@Res() res: Response): Promise<Response> {
        // Readiness probe: 200 OK only for Active Leader (so K8s Service routes client traffic only to Leader)
        const isLeader = this.leaderElectionService.isLeader;

        if (isLeader) {
            try {
                const xrayStatus = await this.xrayProcessService.getStatus();
                if (!xrayStatus.up) {
                    return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
                        status: 'degraded',
                        role: 'leader',
                        xray: 'down',
                    });
                }

                return res.status(HttpStatus.OK).json({
                    status: 'ok',
                    role: 'leader',
                    xray: 'up',
                    pid: xrayStatus.pid,
                    identity: this.leaderElectionService.identity,
                });
            } catch (error) {
                return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
                    status: 'error',
                    message: error instanceof Error ? error.message : String(error),
                });
            }
        }

        // Return 503 for Standby follower so it is excluded from Service Endpoints
        return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
            status: 'standby',
            role: 'follower',
            identity: this.leaderElectionService.identity,
            message: 'Hot-standby replica. Client traffic routed to active leader.',
        });
    }
}
