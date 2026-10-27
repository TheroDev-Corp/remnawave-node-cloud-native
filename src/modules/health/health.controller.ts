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
        const isServing = this.leaderElectionService.isServingTraffic;
        const isDraining = this.leaderElectionService.isDraining;

        let xrayUp = false;
        let xrayPid: number | null = null;

        try {
            const xrayStatus = await this.xrayProcessService.getStatus();
            xrayUp = xrayStatus.up;
            xrayPid = xrayStatus.pid;
        } catch {
            // Xray may not be started yet on initial boot before panel pushes config
        }

        const role = isLeader ? 'leader' : isDraining ? 'leader-draining' : 'standby';

        // A leader (or draining leader) is always Ready so the panel can reach it and push the initial config.
        // A standby is Ready only once its Xray is up (synced from the leader). This makes a rolling update
        // wait for the new pod to be a real hot standby before the old leader is terminated.
        // Traffic routing to leader is strictly handled by K8s Service selector (role: leader).
        const ready = isLeader || isDraining || xrayUp;
        return res.status(ready ? HttpStatus.OK : HttpStatus.SERVICE_UNAVAILABLE).json({
            status: ready ? 'ok' : 'standby-xray-not-ready',
            role,
            servingTraffic: isServing,
            draining: isDraining,
            xray: xrayUp ? 'up' : 'down',
            pid: xrayPid,
            identity: this.leaderElectionService.identity,
        });
    }
}
