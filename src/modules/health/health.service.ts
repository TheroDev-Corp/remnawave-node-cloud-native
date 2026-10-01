import express, { Express, Request, Response } from 'express';
import { Server } from 'node:http';

import { Injectable, Logger, OnApplicationShutdown, OnModuleInit } from '@nestjs/common';

import { TypedConfigService } from '@common/config/app-config/typed-config.service';

import { LeaderElectionService } from '../leader-election/leader-election.service';
import { XrayProcessService } from '../xray-core/xray-process.service';

@Injectable()
export class HealthService implements OnModuleInit, OnApplicationShutdown {
    private readonly logger = new Logger(HealthService.name);
    private server?: Server;

    constructor(
        private readonly config: TypedConfigService,
        private readonly leaderElectionService: LeaderElectionService,
        private readonly xrayProcessService: XrayProcessService,
    ) {}

    onModuleInit(): void {
        const port = this.config.get('HEALTH_PORT');
        if (!port) {
            return;
        }

        const app: Express = express();

        app.get('/health/live', async (_req: Request, res: Response) => {
            return res.status(200).json({
                status: 'ok',
                role: this.leaderElectionService.isLeader ? 'leader' : 'follower',
                identity: this.leaderElectionService.identity,
            });
        });

        app.get('/health/ready', async (_req: Request, res: Response) => {
            if (!this.leaderElectionService.isLeader) {
                return res.status(503).json({
                    status: 'standby',
                    role: 'follower',
                    identity: this.leaderElectionService.identity,
                    message: 'Standby replica. Client traffic routed to active leader.',
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

            return res.status(200).json({
                status: 'ok',
                role: 'leader',
                xray: xrayUp ? 'up' : 'down',
                pid: xrayPid,
                identity: this.leaderElectionService.identity,
            });
        });

        this.server = app.listen(port, '0.0.0.0', () => {
            this.logger.log(`Plain HTTP Health check server listening on port ${port}`);
        });
    }

    onApplicationShutdown(): void {
        if (this.server) {
            this.server.close();
        }
    }
}
