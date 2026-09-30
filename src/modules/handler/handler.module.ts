import { Module } from '@nestjs/common';
import { CqrsModule } from '@nestjs/cqrs';

import { COMMANDS } from './commands';
import { HandlerController } from './handler.controller';
import { HandlerService } from './handler.service';
@Module({
    imports: [CqrsModule],
    controllers: [HandlerController],
    providers: [HandlerService, ...COMMANDS],
})
export class HandlerModule {}
