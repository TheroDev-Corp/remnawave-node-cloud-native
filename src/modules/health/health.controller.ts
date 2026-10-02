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
        return res.status(HttpStatus.OK).json({
            status: 'ok',
            role: this.leaderElectionService.isLeader ? 'leader' : 'follower',
            identity: this.leaderElectionService.identity,
        });
    }

    @Get('ready')
    async ready(@Res() res: Response): Promise<Response> {
        const isLeader = this.leaderElectionService.isLeader;

        if (!isLeader) {
            return res.status(HttpStatus.SERVICE_UNAVAILABLE).json({
                status: 'standby',
                role: 'follower',
                identity: this.leaderElectionService.identity,
                message: 'Hot-standby replica. Client traffic routed to active leader.',
            });
        }

        let xrayUp = false;
        let xrayPid: number | null = null;

        try {
            const xrayStatus = await this.xrayProcessService.getStatus();
            xrayUp = xrayStatus.up;
            xrayPid = xrayStatus.pid;
        } catch {
            // Xray may not be started yet on initial boot before panel pushes config
        }

        return res.status(HttpStatus.OK).json({
            status: 'ok',
            role: 'leader',
            xray: xrayUp ? 'up' : 'down',
            pid: xrayPid,
            identity: this.leaderElectionService.identity,
        });
    }
}
