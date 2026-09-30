import { createZodDto } from 'nestjs-zod';
import { z } from 'zod';

import { parseNodePayloadFromConfigService } from '@common/utils/decode-node-payload';

const booleanString = (def: 'true' | 'false' = 'false') =>
    z
        .string()
        .default(def)
        .transform((val) => (val === '' ? def : val))
        .refine((val) => val === 'true' || val === 'false', 'Must be "true" or "false".')
        .transform((val) => val === 'true')
        .pipe(z.boolean());

export const configSchema = z
    .object({
        NODE_PORT: z.string().transform((port) => {
            return parseInt(port, 10);
        }),
        HEALTH_PORT: z
            .string()
            .optional()
            .transform((port) => (port ? parseInt(port, 10) : undefined)),
        SECRET_KEY: z.string(),
        JWT_PUBLIC_KEY: z.string().optional(),
        DISABLE_HASHED_SET_CHECK: booleanString(),
        INTERNAL_REST_TOKEN: z.string(),
        INTERNAL_SOCKET_PATH: z.string(),
        XTLS_API_SOCKET_PATH: z.string(),
        NFTABLES_LOGGING: booleanString('true'),
        NFTABLES_ACCEPT_REPLY_TRAFFIC: booleanString('false'),
        SNI_VERIFICATION: booleanString('false'),
        K8S_LEADER_ELECTION_ENABLED: booleanString('false'),
        K8S_LEASE_NAME: z.string().default('remnanode-leader'),
        PEER_HEADLESS_SERVICE: z.string().optional(),
        TRUSTED_PROXIES: z.string().default('10.0.0.0/8,172.16.0.0/12,192.168.0.0/16'),
        DISABLE_INSTANCE_LOCK: booleanString('false'),
        TRAEFIK_INTEGRATION_ENABLED: booleanString('false'),
        TRAEFIK_MIDDLEWARE_NAME: z.string().default('reality-whitelist'),
    })

    .superRefine((data, ctx) => {
        if (data.SECRET_KEY) {
            try {
                const parsed = parseNodePayloadFromConfigService(data.SECRET_KEY);
                data.JWT_PUBLIC_KEY = parsed.jwtPublicKey;
            } catch {
                ctx.issues.push({
                    code: 'custom',
                    input: data.SECRET_KEY,
                    message: 'Invalid SECRET_KEY payload',
                });
            }
        }
    });

export type ConfigSchema = z.infer<typeof configSchema>;
export class Env extends createZodDto(configSchema) {}
