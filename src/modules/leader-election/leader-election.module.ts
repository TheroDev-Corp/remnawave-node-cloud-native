import { Global, Module } from '@nestjs/common';
import { CqrsModule } from '@nestjs/cqrs';

import { LeaderElectionService } from './leader-election.service';

@Global()
@Module({
    imports: [CqrsModule],
    providers: [LeaderElectionService],
    exports: [LeaderElectionService],
})
export class LeaderElectionModule {}
