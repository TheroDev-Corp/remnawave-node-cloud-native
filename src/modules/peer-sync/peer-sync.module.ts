import { forwardRef, Global, Module } from '@nestjs/common';
import { CqrsModule } from '@nestjs/cqrs';

import { HandlerModule } from '../handler/handler.module';
import { InternalModule } from '../internal/internal.module';
import { LeaderElectionModule } from '../leader-election/leader-election.module';
import { XrayModule } from '../xray-core/xray.module';
import { LeaderPromotedHandler } from './events/leader-promoted.handler';
import { PeerSyncController } from './peer-sync.controller';
import { PeerSyncService } from './peer-sync.service';

@Global()
@Module({
    imports: [
        CqrsModule,
        LeaderElectionModule,
        InternalModule,
        forwardRef(() => XrayModule),
        forwardRef(() => HandlerModule),
    ],
    controllers: [PeerSyncController],
    providers: [PeerSyncService, LeaderPromotedHandler],
    exports: [PeerSyncService],
})
export class PeerSyncModule {}
