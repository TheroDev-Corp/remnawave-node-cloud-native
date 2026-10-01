import { Logger, Module, OnApplicationShutdown } from '@nestjs/common';

import { IntegrationsModule } from '@integration-modules/integrations.module';

import { PluginModule } from './_plugin/plugin.module';
import { AsnLmdbModule } from './asn-lmdb/asn-lmdb.module';
import { HandlerModule } from './handler/handler.module';
import { HealthModule } from './health/health.module';
import { LeaderElectionModule } from './leader-election/leader-election.module';
import { NetworkStatsModule } from './network-stats/network-stats.module';
import { PeerSyncModule } from './peer-sync/peer-sync.module';
import { StatsModule } from './stats/stats.module';
import { XrayModule } from './xray-core/xray.module';

@Module({
    imports: [
        IntegrationsModule,
        LeaderElectionModule,
        PeerSyncModule,
        HealthModule,
        AsnLmdbModule,
        NetworkStatsModule,
        PluginModule,
        StatsModule,
        XrayModule,
        HandlerModule,
    ],
    providers: [],
})
export class RemnawaveNodeModules implements OnApplicationShutdown {
    private readonly logger = new Logger(RemnawaveNodeModules.name);

    async onApplicationShutdown(signal?: string): Promise<void> {
        this.logger.log(`${signal} received, shutting down...`);
    }
}
