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
            // Liveness probe: returns 200 as long as Node process is running
            // On the leader, we can also check if Xray is running if desired,
            // but standby replicas don't run Xray, so node being alive is sufficient.
            return res.status(200).json({
                status: 'ok',
                role: this.leaderElectionService.isLeader ? 'leader' : 'follower',
            });
        });

        app.get('/health/ready', async (_req: Request, res: Response) => {
            // Readiness probe: 200 OK only for Active Leader (and Xray is up)
            if (!this.leaderElectionService.isLeader) {
                return res.status(503).json({
                    status: 'standby',
                    role: 'follower',
                    identity: this.leaderElectionService.identity,
                    message: 'Standby replica. Client traffic routed to active leader.',
                });
            }

            try {
                const xrayStatus = await this.xrayProcessService.getStatus();
                if (!xrayStatus.up) {
                    return res.status(503).json({
                        status: 'degraded',
                        role: 'leader',
                        xray: 'down',
                    });
                }

                return res.status(200).json({
                    status: 'ok',
                    role: 'leader',
                    xray: 'up',
                    pid: xrayStatus.pid,
                });
            } catch (error) {
                return res.status(503).json({
                    status: 'error',
                    message: error instanceof Error ? error.message : String(error),
                });
            }
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
