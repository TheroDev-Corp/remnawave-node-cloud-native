import { Logger } from '@nestjs/common';
import { IQueryHandler, QueryHandler } from '@nestjs/cqrs';

import { NetworkStatsService } from '../../network-stats.service';
import { GetInterfaceStatsQuery } from './get-interface-stats.query';

@QueryHandler(GetInterfaceStatsQuery)
export class GetInterfaceStatsHandler implements IQueryHandler<GetInterfaceStatsQuery> {
    private readonly logger = new Logger(GetInterfaceStatsHandler.name);
    constructor(private readonly networkStatsService: NetworkStatsService) {}

    async execute() {
        return this.networkStatsService.getDefault();
    }
}
