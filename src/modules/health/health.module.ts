import { Module } from '@nestjs/common';

import { XrayModule } from '../xray-core/xray.module';
import { HealthController } from './health.controller';
import { HealthService } from './health.service';

@Module({
    imports: [XrayModule],
    controllers: [HealthController],
    providers: [HealthService],
    exports: [HealthService],
})
export class HealthModule {}
