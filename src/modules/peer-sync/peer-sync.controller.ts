import type { Response } from 'express';

import {
    Body,
    Controller,
    Get,
    Headers,
    HttpStatus,
    Post,
    Res,
    UnauthorizedException,
} from '@nestjs/common';

import { TypedConfigService } from '@common/config/app-config';

import { PeerSyncService, type TPeerReplicationAction } from './peer-sync.service';

export class PeerReplicationActionDto {
    type!: string;
    body!: unknown;
}

@Controller('internal/peer-sync')
export class PeerSyncController {
    private readonly secretKey: string;

    constructor(
        private readonly peerSyncService: PeerSyncService,
        private readonly configService: TypedConfigService,
    ) {
        this.secretKey = this.configService.getOrThrow('SECRET_KEY');
    }

    @Post('apply')
    async apply(
        @Headers('x-secret-key') authHeader: string,
        @Body() action: PeerReplicationActionDto,
        @Res() res: Response,
    ): Promise<Response> {
        this.verifyAuth(authHeader);

        try {
            await this.peerSyncService.applyReplicatedAction(
                action as unknown as TPeerReplicationAction,
            );
            return res.status(HttpStatus.OK).json({ status: 'applied' });
        } catch (error) {
            return res.status(HttpStatus.INTERNAL_SERVER_ERROR).json({
                status: 'error',
                message: error instanceof Error ? error.message : String(error),
            });
        }
    }

    @Get('snapshot')
    async getSnapshot(
        @Headers('x-secret-key') authHeader: string,
        @Res() res: Response,
    ): Promise<Response> {
        this.verifyAuth(authHeader);

        const snapshot = await this.peerSyncService.exportSnapshot();
        return res.status(HttpStatus.OK).json(snapshot);
    }

    private verifyAuth(authHeader?: string): void {
        if (!authHeader || authHeader !== this.secretKey) {
            throw new UnauthorizedException('Invalid peer sync authentication secret');
        }
    }
}
