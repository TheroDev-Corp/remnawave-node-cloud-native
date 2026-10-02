import { Body, Controller, Post, UseFilters, UseGuards } from '@nestjs/common';

import { HttpExceptionFilter } from '@common/exception';
import { JwtDefaultGuard } from '@common/guards/jwt-guards';
import { errorHandler } from '@common/helpers';
import { PLUGIN_CONTROLLER, PLUGIN_ROUTES } from '@libs/contracts/api';

import {
    BlockIpsResponseDto,
    CollectReportsResponseDto,
    RecreateTablesResponseDto,
    UnblockIpsResponseDto,
} from './dtos';
import { SyncRequestDto, SyncResponseDto } from './dtos/sync.dto';
import { GenericResponseModel } from './models';
import { PluginService } from './plugin.service';

@UseFilters(HttpExceptionFilter)
@UseGuards(JwtDefaultGuard)
@Controller(PLUGIN_CONTROLLER)
export class PluginController {
    constructor(private readonly pluginService: PluginService) {}

    @Post(PLUGIN_ROUTES.SYNC)
    public async sync(@Body() body: SyncRequestDto): Promise<SyncResponseDto> {
        const response = await this.pluginService.sync(body);
        const data = errorHandler(response);

        return {
            response: data,
        };
    }

    @Post(PLUGIN_ROUTES.TORRENT_BLOCKER.COLLECT)
    public async collectReports(): Promise<CollectReportsResponseDto> {
        const response = await this.pluginService.collectReports();
        const data = errorHandler(response);

        return {
            response: data,
        };
    }

    @Post(PLUGIN_ROUTES.NFTABLES.BLOCK_IPS)
    public async blockIps(): Promise<BlockIpsResponseDto> {
        return {
            response: new GenericResponseModel(true),
        };
    }

    @Post(PLUGIN_ROUTES.NFTABLES.UNBLOCK_IPS)
    public async unblockIps(): Promise<UnblockIpsResponseDto> {
        return {
            response: new GenericResponseModel(true),
        };
    }

    @Post(PLUGIN_ROUTES.NFTABLES.RECREATE_TABLES)
    public async recreateTables(): Promise<RecreateTablesResponseDto> {
        return {
            response: new GenericResponseModel(true),
        };
    }
}
