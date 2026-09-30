import { z } from 'zod';

import { REST_API } from '../../../api';
import { TorrentBlockerReportSchema } from '../../../models';

export namespace CollectReportsCommand {
    export const url = REST_API.PLUGIN.TORRENT_BLOCKER.COLLECT;

    export const ResponseSchema = z.object({
        response: z.object({
            reports: z.array(TorrentBlockerReportSchema),
        }),
    });

    export type Response = z.infer<typeof ResponseSchema>;
}
