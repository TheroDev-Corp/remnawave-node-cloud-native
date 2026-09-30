import { Module } from '@nestjs/common';

import { XrayModule } from '../xray-core/xray.module';
import { HealthController } from './health.controller';

@Module({
    imports: [XrayModule],
    controllers: [HealthController],
})
export class HealthModule {}
