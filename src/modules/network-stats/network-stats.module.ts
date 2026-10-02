import { Module } from '@nestjs/common';
import { CqrsModule } from '@nestjs/cqrs';

import { NetworkStatsService } from './network-stats.service';
import { QUERIES } from './queries';

@Module({
    imports: [CqrsModule],
    providers: [NetworkStatsService, ...QUERIES],
    exports: [NetworkStatsService],
})
export class NetworkStatsModule {}
