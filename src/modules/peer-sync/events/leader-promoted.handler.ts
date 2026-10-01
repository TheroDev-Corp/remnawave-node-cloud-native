import { Injectable, Logger } from '@nestjs/common';
import { EventsHandler, IEventHandler } from '@nestjs/cqrs';

import { LeaderPromotedEvent } from '../../leader-election/events/leader-promoted.event';
import { PeerSyncService } from '../peer-sync.service';

@Injectable()
@EventsHandler(LeaderPromotedEvent)
export class LeaderPromotedHandler implements IEventHandler<LeaderPromotedEvent> {
    private readonly logger = new Logger(LeaderPromotedHandler.name);

    constructor(private readonly peerSyncService: PeerSyncService) {}

    async handle(event: LeaderPromotedEvent): Promise<void> {
        this.logger.log(`Handling LeaderPromotedEvent for pod "${event.holderIdentity}"`);
        await this.peerSyncService.onPromotedToLeader();
    }
}
