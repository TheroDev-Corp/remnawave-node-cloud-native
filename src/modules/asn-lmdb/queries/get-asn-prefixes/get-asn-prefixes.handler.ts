import { Logger } from '@nestjs/common';
import { IQueryHandler, QueryHandler } from '@nestjs/cqrs';

import { AsnLmdbService } from '../../asn-lmdb.service';
import { GetAsnPrefixesQuery } from './get-asn-prefixes.query';

@QueryHandler(GetAsnPrefixesQuery)
export class GetAsnPrefixesHandler implements IQueryHandler<GetAsnPrefixesQuery> {
    private readonly logger = new Logger(GetAsnPrefixesHandler.name);
    constructor(private readonly asnLmdbService: AsnLmdbService) {}

    async execute(query: GetAsnPrefixesQuery) {
        return this.asnLmdbService.getByAsn(query.asn);
    }
}
