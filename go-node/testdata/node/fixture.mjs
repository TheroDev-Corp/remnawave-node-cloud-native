// Cross-checks the Go node against the TS node's own code and packages.
//   echo '{"op":"..."}' | node --import ./ts-hooks.mjs fixture.mjs
// Run from anywhere; paths resolve relative to the repo root (../../..).
// Needs `npm ci` in the repo root (zod, @remnawave/hashed-set, node-object-hash).
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { constants as zc, zstdCompressSync } from 'node:zlib';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, '..', '..', '..');
const require = createRequire(path.join(repo, 'package.json'));
const ts = (rel) => import(path.join(repo, rel));

async function contract() {
    const c = await ts('libs/contract/index.ts');
    return Object.fromEntries(Object.entries(c).filter(([k, v]) => k.endsWith('Command') && v?.url));
}

// Keys present in `input` but not in zod's output were stripped as unknown.
function unknownKeys(input, output, at = '') {
    if (input === null || typeof input !== 'object' || output === null || typeof output !== 'object') return [];
    if (Array.isArray(input)) return input.flatMap((v, i) => unknownKeys(v, output?.[i], `${at}[${i}]`));
    return Object.keys(input).flatMap((k) =>
        k in output ? unknownKeys(input[k], output[k], at ? `${at}.${k}` : k) : [at ? `${at}.${k}` : k],
    );
}

const req = JSON.parse(readFileSync(0, 'utf8'));
let out;
switch (req.op) {
    case 'schemas': {
        // JSON Schema of every command's request and response, for the golden files.
        out = {};
        for (const [name, cmd] of Object.entries(await contract())) {
            out[name] = {
                url: typeof cmd.url === 'function' ? cmd.url(':param') : cmd.url,
                request: cmd.RequestSchema ? cmd.RequestSchema.toJSONSchema({ unrepresentable: 'any' }) : null,
                response: cmd.ResponseSchema.toJSONSchema({ unrepresentable: 'any' }),
            };
        }
        break;
    }
    case 'pluginSchema': {
        // @remnawave/node-plugins' NodePluginSchema (input side, with defaults).
        const { NodePluginSchema } = require('@remnawave/node-plugins');
        out = NodePluginSchema.toJSONSchema({ unrepresentable: 'any', io: 'input' });
        break;
    }
    case 'check': {
        // Validate a JSON value against a command's schema, like the panel's
        // contract types (and the TS node's ZodValidationPipe) would.
        const cmd = (await contract())[req.command];
        const schema = req.kind === 'request' ? cmd.RequestSchema : cmd.ResponseSchema;
        const r = schema.safeParse(req.value);
        out = r.success
            ? { ok: true, unknown: unknownKeys(req.value, r.data) }
            : { ok: false, issues: r.error.issues.map((i) => `${i.path.join('.')}: ${i.message}`) };
        break;
    }
    case 'checkMany': {
        // Batch of {command, kind, value}; same result shape as 'check'.
        const cmds = await contract();
        out = req.items.map(({ command, kind, value }) => {
            const cmd = cmds[command];
            const schema = kind === 'request' ? cmd.RequestSchema : cmd.ResponseSchema;
            const r = schema.safeParse(value);
            return r.success
                ? { ok: true, unknown: unknownKeys(value, r.data) }
                : { ok: false, issues: r.error.issues.map((i) => `${i.path.join('.')}: ${i.message}`) };
        });
        break;
    }
    case 'zodIssues': {
        // Batch of {command, value}: the request issues exactly as the TS
        // node's ZodValidationPipe puts them in a 400 body.
        const cmds = await contract();
        out = req.items.map(({ command, value }) => {
            const r = cmds[command].RequestSchema.safeParse(value);
            return r.success ? [] : JSON.parse(JSON.stringify(r.error.issues));
        });
        break;
    }
    case 'negotiate': {
        // compression@1.8.1's choice for each Accept-Encoding header.
        const Negotiator = require(require.resolve('negotiator', { paths: [require.resolve('compression')] }));
        out = req.headers.map((h) => new Negotiator({ headers: { 'accept-encoding': h } }).encoding(['br', 'gzip', 'deflate', 'identity'], ['br', 'gzip']) ?? '');
        break;
    }
    case 'asnDump': {
        // Every entry of the ASN database, as the TS node's lmdb-js reads it.
        const { open } = require('lmdb');
        const db = open({ path: req.path, encoding: 'msgpack', readOnly: true });
        out = [];
        for (const { key, value } of db.getRange()) out.push([key, value.ipv4, value.ipv6]);
        db.close();
        break;
    }
    case 'semverGte': {
        const semver = require('semver');
        out = req.versions.map((v) => semver.valid(v) !== null && !semver.lt(v, req.min));
        break;
    }
    case 'deriveSni': {
        const { deriveSni } = await ts('src/common/utils/decode-node-payload/decode-servername.util.ts');
        out = deriveSni(req.ca, req.jwt);
        break;
    }
    case 'normalizePem': {
        // normalizePem is module-private in decode-node-payload.ts: evaluate
        // its source text so the test runs the real implementation.
        const src = readFileSync(path.join(repo, 'src/common/utils/decode-node-payload/decode-node-payload.ts'), 'utf8');
        const fn = src.match(/function normalizePem\(pem: string\): string \{[\s\S]*?\n\}/)[0].replace('pem: string): string', 'pem)');
        const normalizePem = new Function(`${fn}; return normalizePem;`)();
        out = req.values.map(normalizePem);
        break;
    }
    case 'zstd': {
        // Exactly what the panel's AxiosService.compressData sends.
        const buffer = Buffer.from(JSON.stringify(req.value ?? JSON.parse(readFileSync(req.in, 'utf8'))));
        const z = zstdCompressSync(buffer, {
            params: {
                [zc.ZSTD_c_compressionLevel]: 1,
                [zc.ZSTD_c_enableLongDistanceMatching]: 1,
                [zc.ZSTD_c_windowLog]: 25,
            },
            chunkSize: 1024 * 1024,
            pledgedSrcSize: buffer.length,
        });
        writeFileSync(req.out, z);
        out = { raw: buffer.length, compressed: z.length };
        break;
    }
    case 'hashedSet': {
        const { HashedSet } = require('@remnawave/hashed-set');
        out = req.sets.map((values) => new HashedSet(values).hash64String);
        break;
    }
    case 'hashedSetOps': {
        // Replays add/delete sequences, returning the hash after each step.
        const { HashedSet } = require('@remnawave/hashed-set');
        const s = new HashedSet();
        out = req.ops.map(([op, v]) => {
            op === 'add' ? s.add(v) : s.delete(v);
            return s.hash64String;
        });
        break;
    }
    case 'objectHash': {
        const hasher = require('node-object-hash')(req.options ?? {});
        out = req.values.map((v) => hasher.hash(v));
        break;
    }
    default:
        throw new Error('unknown op ' + req.op);
}
process.stdout.write(JSON.stringify(out));
