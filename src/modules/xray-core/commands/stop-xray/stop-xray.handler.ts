import { Logger } from '@nestjs/common';
import { CommandHandler, ICommandHandler } from '@nestjs/cqrs';

import { XrayService } from '../../xray.service';
import { StopXrayCommand } from './stop-xray.command';

@CommandHandler(StopXrayCommand)
export class StopXrayHandler implements ICommandHandler<StopXrayCommand> {
    public readonly logger = new Logger(StopXrayHandler.name);

    constructor(private readonly xrayService: XrayService) {}

    async execute(command: StopXrayCommand) {
        try {
            await this.xrayService.stopXray(command.args);

            return;
        } catch (error) {
            this.logger.error(error);
            return;
        }
    }
}
