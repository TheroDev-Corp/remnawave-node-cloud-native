import { forwardRef, Module } from '@nestjs/common';
import { CqrsModule } from '@nestjs/cqrs';

import { HandlerModule } from '../handler/handler.module';
import { InternalModule } from '../internal/internal.module';
import { PeerSyncModule } from '../peer-sync/peer-sync.module';
import { COMMANDS } from './commands';
import { EVENTS } from './events';
import { PluginController } from './plugin.controller';
import { PluginService } from './plugin.service';
import { QUERIES } from './queries';
import { NftService } from './services/nft.service';
import { PluginStateService } from './services/plugin-state.service';
import { PreStartService } from './services/pre-start.service';
import { UserSuspensionService } from './services/user-suspension.service';

@Module({
    imports: [
        CqrsModule,
        InternalModule,
        forwardRef(() => HandlerModule),
        forwardRef(() => PeerSyncModule),
    ],
    controllers: [PluginController],
    providers: [
        PluginService,
        PluginStateService,
        UserSuspensionService,
        NftService,
        PreStartService,
        ...QUERIES,
        ...EVENTS,
        ...COMMANDS,
    ],
    exports: [PluginStateService, UserSuspensionService],
})
export class PluginModule {}
