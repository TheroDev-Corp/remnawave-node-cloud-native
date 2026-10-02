import { Logger } from '@nestjs/common';
import { CommandHandler, ICommandHandler } from '@nestjs/cqrs';

import { PluginService } from '../../plugin.service';
import { ResetPluginsCommand } from './reset-plugins.command';

@CommandHandler(ResetPluginsCommand)
export class ResetPluginsHandler implements ICommandHandler<ResetPluginsCommand> {
    public readonly logger = new Logger(ResetPluginsHandler.name);

    constructor(private readonly pluginService: PluginService) {}

    async execute() {
        try {
            await this.pluginService.resetPlugins();
            return;
        } catch (error) {
            this.logger.error(error);
            return;
        }
    }
}
