import { Global, Module } from '@nestjs/common';

import { TraefikService } from './traefik.service';

@Global()
@Module({
    providers: [TraefikService],
    exports: [TraefikService],
})
export class TraefikModule {}
