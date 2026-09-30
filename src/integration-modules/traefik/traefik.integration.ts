import { Injectable, Logger } from '@nestjs/common';

import {
    INodeIntegration,
    INodeIntegrationDescriptor,
    INodeIntegrationResult,
    INodeIntegrationStartOptions,
} from '../integrations.contract';
import { TraefikModule } from './traefik.module';
import { TraefikService } from './traefik.service';

@Injectable()
export class TraefikIntegration implements INodeIntegration {
    readonly name = 'traefik';
    private readonly logger = new Logger(TraefikIntegration.name);

    constructor(private readonly traefikService: TraefikService) {}

    public async sync(_options: INodeIntegrationStartOptions): Promise<INodeIntegrationResult> {
        if (!this.traefikService.isEnabled) {
            return { error: null };
        }

        this.logger.log('[TRAEFIK-INTEGRATION] Synced Traefik middleware integration.');
        return { error: null };
    }

    public async stop(): Promise<void> {
        this.logger.log('[TRAEFIK-INTEGRATION] Stopping Traefik integration...');
    }
}

export const descriptor: INodeIntegrationDescriptor = {
    module: TraefikModule,
    service: TraefikIntegration,
    isAvailable: () => true,
};
